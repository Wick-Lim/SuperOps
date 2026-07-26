// Command worker runs SuperOps' asynchronous half: the JetStream consumers that
// index messages for search and fan out notifications, plus the periodic
// maintenance jobs (session cleanup, scheduled-message promotion, retention
// purge, orphaned-object GC).
//
// Two invariants shape this file:
//
//   - Event consumers are durable JetStream consumers with explicit acks, not
//     core-NATS subscriptions. A core subscription drops every event published
//     while the worker is restarting, and those events are the only trigger for
//     search indexing and notifications — there is no reconciliation pass that
//     would ever notice. For the same reason an ack has to mean the work
//     happened: handlers return an error, and only a nil error acks. See
//     bindDurable.
//   - Every job that mutates shared state at scale takes a Postgres advisory
//     lock, so scaling the Deployment to three replicas does not mean three
//     replicas racing on the same DELETE.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/channel"
	"github.com/Wick-Lim/SuperOps/backend/internal/collab"
	"github.com/Wick-Lim/SuperOps/backend/internal/comment"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/huddle"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/mailbox"
	"github.com/Wick-Lim/SuperOps/backend/internal/message"
	"github.com/Wick-Lim/SuperOps/backend/internal/notification"
	"github.com/Wick-Lim/SuperOps/backend/internal/push"
	"github.com/Wick-Lim/SuperOps/backend/internal/quota"
	"github.com/Wick-Lim/SuperOps/backend/internal/rtc"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
	"github.com/Wick-Lim/SuperOps/backend/internal/sso"
	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
	"github.com/Wick-Lim/SuperOps/backend/internal/thumb"
	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/internal/workflow"
	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// Job names, also used as the keys in the /health payload.
const (
	jobSessionCleanup   = "session_cleanup"
	jobScheduledSend    = "scheduled_messages"
	jobRetention        = "retention"
	jobObjectGC         = "object_gc"
	jobACLDrift         = "acl_drift"
	jobInboxDigest      = "inbox_digest"
	jobInboxReconcile   = "inbox_reconcile"
	jobAuditPartition   = "audit_partitions"
	jobAuditVerify      = "audit_verify"
	jobDriveTrash       = "drive_trash_purge"
	jobHuddleReconcile  = "huddle_reconcile"
	jobWorkflowStep     = "workflow_step"
	jobWorkflowReaper   = "workflow_reaper"
	jobMailSweep        = "mail_unsent_sweep"
	jobProjectionRepair = "projection_repair"
	jobQuotaDrift       = "quota_drift"
)

// Job cadences. startDelay staggers the first run so a rolling restart does not
// have every replica hit Postgres with its heaviest query at the same instant.
const (
	sessionCleanupInterval = 10 * time.Minute
	scheduledInterval      = 30 * time.Second
	retentionInterval      = time.Hour
	retentionStartDelay    = 2 * time.Minute
	objectGCInterval       = time.Hour
	objectGCStartDelay     = 5 * time.Minute
	aclDriftInterval       = time.Hour
	aclDriftStartDelay     = 3 * time.Minute

	// Quota drift. Hourly like acl_drift, and offset from it so two full-table
	// sweeps do not start together.
	quotaDriftInterval     = time.Hour
	quotaDriftStartDelay   = 7 * time.Minute
	inboxDigestInterval    = 5 * time.Minute
	inboxDigestStartDelay  = 90 * time.Second
	inboxReconcileInterval = 15 * time.Minute
	inboxReconcileDelay    = 4 * time.Minute
	// Hourly, and the lead time is two months, so a run that is missed — or a
	// worker that is down for a week — cannot reach a month with no partition.
	auditPartitionInterval = time.Hour
	auditPartitionDelay    = 30 * time.Second
	auditVerifyInterval    = 30 * time.Minute
	auditVerifyStartDelay  = 6 * time.Minute
	// Hourly. The retention window is measured in days, so the cadence only has
	// to be short enough that "30 days" is not "30 days and a bit"; running more
	// often would scan the same empty index over and over.
	driveTrashInterval   = time.Hour
	driveTrashStartDelay = 7 * time.Minute
	// Huddles, every 30 seconds. Short because the cost of being wrong is a
	// channel whose Huddle button does nothing — the partial unique index makes
	// a stale live row block every new call on that scope — and because asking
	// the media server about a handful of live rooms is cheap. The grace period
	// keeps it from ending a call that was created a second ago and whose first
	// participant has not connected yet.
	huddleReconcileInterval   = 30 * time.Second
	huddleReconcileStartDelay = 45 * time.Second
	huddleReconcileGrace      = 60 * time.Second
	// Workflows. The step loop is fast because ClaimRun is a single indexed
	// SKIP LOCKED query that returns immediately when the queue is empty.
	workflowStepInterval   = 2 * time.Second
	workflowStepStartDelay = 5 * time.Second
	workflowReaperInterval = 60 * time.Second
	workflowReaperDelay    = 90 * time.Second
	// A run claimed and untouched for this long belongs to a worker that died.
	// Longer than any legitimate run: the steps are chat posts and inbox
	// publishes, not long computations.
	workflowRunStale = 5 * time.Minute
	// One claim loop drains at most this many runs before yielding, so a large
	// backlog cannot starve the other job loops on this replica.
	workflowStepBatch = 50
	// Every five minutes. The queue is the normal path; this is the backstop,
	// and its own grace period keeps it from racing a message queued moments
	// ago.
	mailSweepInterval   = 5 * time.Minute
	mailSweepStartDelay = 2 * time.Minute

	// Projection repair. Slow on purpose: the healthy paths — the editor's
	// debounce, its unmount flush, and the catch-up on open — repair almost
	// everything, and this exists for what they miss. Running it often would
	// mean asking rooms for snapshots that a debounce two seconds away was
	// about to produce anyway.
	projectionRepairInterval   = 10 * time.Minute
	projectionRepairStartDelay = 3 * time.Minute
	// How far behind the log a projection must be before it is worth a request.
	// Below this the gap is ordinary in-flight editing.
	projectionRepairGap = 200
	// And how long it must have been behind. A document being actively typed in
	// is always somewhat behind; one untouched for this long is not mid-edit.
	projectionRepairAge = 30 * time.Minute
	// Per sweep. A bounded, visible amount of work rather than a thundering
	// request to every room in a backlog.
	projectionRepairBatch = 100
)

// Advisory-lock keys. Distinct 64-bit constants; any other application taking
// pg_advisory_lock on the same database must avoid these.
const (
	lockSessionCleanup int64 = 0x50_0001
	lockRetention      int64 = 0x50_0002
	lockObjectGC       int64 = 0x50_0003
	lockACLDrift       int64 = 0x50_0004
	// 0x50_0004 was already taken by acl_drift when plan 01 was written, which
	// allocated it to inbox_digest. These take the next free values instead;
	// two jobs sharing an advisory lock would silently serialise them.
	lockInboxDigest    int64 = 0x50_0005
	lockInboxReconcile int64 = 0x50_0006
	lockAuditPartition int64 = 0x50_0007
	lockAuditVerify    int64 = 0x50_0008
	lockDriveTrash     int64 = 0x50_0009
	// Huddles. The reconciler is the only thing that ends a call whose
	// room the media server has forgotten — a webhook that never arrived
	// leaves a huddle live forever, and the partial unique index then blocks
	// every future call on that channel.
	lockHuddleReconcile int64 = 0x50_000A
	// The workflow reaper. It is the only thing that releases a run whose
	// worker died mid-execution; without it that run is stuck 'running'
	// forever and nothing else would ever notice.
	//
	// The STEP loop deliberately takes no lock — it is FOR UPDATE SKIP LOCKED,
	// so several workers share the queue. A lock there would serialize every
	// workflow in the deployment behind one replica.
	lockWorkflowReaper int64 = 0x50_000B
	// The unsent-mail sweep. Locked because it SENDS: two replicas sweeping
	// concurrently would each load the same unstamped row and deliver it twice.
	lockMailSweep int64 = 0x50_000C
	// The projection repair sweep. Locked because it PUBLISHES: two replicas
	// sweeping the same stale documents would ask each room's leader twice for
	// the same snapshot.
	lockProjectionRepair int64 = 0x50_000D
	// The storage-quota reconciler. Locked because it WRITES workspace_storage;
	// two replicas recomputing the same workspace would race on the same row.
	lockQuotaDrift int64 = 0x50_000E
)

// aclDriftSamples bounds how many concrete disagreements one drift report
// names. Enough to debug from a log line, not enough to fill a disk when a
// backfill has genuinely not been run.
const aclDriftSamples = 20

const (
	// consumerMaxDeliver bounds redelivery. Without it a permanently poisonous
	// message is redelivered forever and starves the consumer.
	consumerMaxDeliver = 5
	consumerAckWait    = 30 * time.Second
	consumerMaxPending = 256

	// handlerTimeout bounds one callback. It sits under consumerAckWait so a
	// wedged handler gives up before the server decides the message was never
	// acked and hands a second copy to another replica.
	handlerTimeout = consumerAckWait - 5*time.Second

	// termReasonMax keeps the TERM advisory readable; the full error goes to the
	// log and to /health.
	termReasonMax = 200
)

// consumerBackOff is both the consumer's AckWait schedule (delivery n waits
// backoff[n] before the server assumes the ack was lost) and the delay this
// worker asks for when it naks explicitly. A bare Nak() redelivers immediately,
// which for "Meilisearch is down" means five deliveries burned inside a second
// and the event terminated while the outage is still a few seconds old.
var consumerBackOff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, time.Minute}

const (
	retentionBatchSize  = 500
	retentionMaxBatches = 40 // ceiling per tick: 20k messages, then wait for the next hour

	scheduledPageSize = 200
)

// drainTimeout bounds the whole shutdown: consumer drain, in-flight handlers
// and job loops.
const drainTimeout = 25 * time.Second

func main() { os.Exit(run()) }

func run() int {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Print("load config: ", err)
		return 1
	}

	l := logger.New(cfg.LogLevel)
	l.Info("starting worker")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := database.NewPool(ctx, database.Config{
		DSN:                      cfg.DB.DSN(),
		MaxConns:                 cfg.DB.MaxConns,
		MinConns:                 cfg.DB.MinConns,
		StatementTimeout:         cfg.DB.StatementTimeout,
		LockTimeout:              cfg.DB.LockTimeout,
		IdleInTransactionTimeout: cfg.DB.IdleInTransactionTimeout,
	}, l)
	if err != nil {
		l.Error("database", "error", err)
		return 1
	}
	defer pool.Close()

	natsClient, err := natspkg.NewClient(natspkg.Config{
		URL:          cfg.NATS.URL,
		DrainTimeout: cfg.NATS.DrainTimeout,
	}, l)
	if err != nil {
		l.Error("nats", "error", err)
		return 1
	}
	defer natsClient.Close()

	// The stream is shared with the API, which also creates it (see
	// app.EnsureEventStream). Without it there is nothing to bind a durable
	// consumer to, and the worker would sit idle while looking healthy — so
	// this is fatal rather than a warning. It is retried first: NewClient
	// returns before the connection is necessarily established (it is
	// configured with RetryOnFailedConnect), and in compose/Kubernetes the
	// worker and NATS start at the same moment.
	if err := ensureStreamWithRetry(ctx, natsClient, l); err != nil {
		l.Error("JetStream stream unavailable; refusing to start with no durable consumers", "error", err)
		return 1
	}

	az := authz.New(pool)
	health := newHealthState()

	// --- event consumers -----------------------------------------------------

	// handlers tracks callbacks that are mid-flight so shutdown can wait for
	// them instead of tearing down the pool underneath a half-written fan-out.
	var handlers sync.WaitGroup
	var consumers []jetstream.ConsumeContext

	bind := func(spec durableSpec) {
		cc, err := bindDurable(ctx, natsClient, l, &handlers, health, spec)
		if err != nil {
			l.Error("bind durable consumer", "durable", spec.durable, "error", err)
			return
		}
		consumers = append(consumers, cc)
		health.register(spec.durable, 0) // event driven: idleness is not a fault
		l.Info("durable consumer bound", "durable", spec.durable, "filter", spec.filter)
	}

	var searchSvc *search.Service
	if cfg.Meili.IsEnabled() {
		searchSvc, err = search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, l)
		if err != nil {
			l.Warn("meilisearch not available, search indexing disabled", "error", err)
			searchSvc = nil
		}
	} else {
		l.Info("search disabled by configuration")
	}
	if searchSvc != nil {
		// The body source is the Drive repository: a document's searchable text
		// is the client-published projection, read from the database rather than
		// carried on the event. A registry with no kinds is deliberate here —
		// this repository is used for one query that touches no kind at all, and
		// giving the worker a second registration list would make "which editors
		// exist" a thing two processes could disagree about.
		bodies := drive.NewRepository(pool, az, registry.New())
		indexer := search.NewIndexer(searchSvc, az, bodies, l)
		bind(durableSpec{durable: "indexer", filter: "superops.*.message.*", handle: indexer.HandleMessage})
		// Files were never indexed. search.Indexer has had HandleFile since the
		// search feature shipped and nothing ever bound it, so uploading a file
		// put nothing in the index — the handler existed, the events existed, and
		// the two were never introduced. Its own durable rather than a widened
		// message filter: a poisonous file event must not stall message
		// indexing, and the two have independent redelivery budgets.
		bind(durableSpec{durable: "file-indexer", filter: "superops.*.file.*", handle: indexer.HandleFile})
	}

	// Push rides the notification fan-out: the same events, the same audience
	// (muted / notification_pref / blocks are all applied before this point),
	// one extra delivery channel for the recipients who are not looking at a
	// screen. It lives in the worker rather than the API because the worker is
	// where notifications are created; the API never sees the fan-out.
	devices, pusher, pushDispatcher := buildPush(cfg, pool, l)

	// The unified inbox. internal/notification is now only the MESSAGE-DOMAIN
	// producer — DM roster, mention extraction, thread parent, block list,
	// channel mute — and hands every event to this notifier, which owns the
	// coalescing, the idempotency gate, the badge and the preferences for every
	// pillar (docs/plans/README.md "Resolved conflicts" §1).
	inboxRepo := inbox.NewRepository(pool)
	notifier := inbox.NewNotifier(inboxRepo, natsClient, devices, pusher, l)

	notifSvc := notification.NewService(pool, az, notifier, l)
	bind(durableSpec{durable: "notifier-message", filter: "superops.*.message.created", handle: notifSvc.HandleMessage})
	bind(durableSpec{durable: "notifier-reaction", filter: "superops.*.reaction.added", handle: notifSvc.HandleReaction})
	bind(durableSpec{durable: "notifier-channel-invite", filter: "superops.*.channel.member_added", handle: notifSvc.HandleChannelMemberAdded})

	// The pillar-neutral entry point. A pillar that wants to file into the inbox
	// publishes superops.{ws}.inbox.requested with a kind and an explicit
	// recipient list; this one durable serves all of them, so adding a pillar
	// costs zero durables and zero lines in this file.
	bind(durableSpec{durable: inbox.DurableFanout, filter: inbox.FilterRequested, handle: notifier.HandleRequested})

	// Unread badges. Deliberately a separate durable from notifier-message
	// rather than another branch inside it: they have different audiences (a
	// muted channel still accrues unread, it just does not buzz) and different
	// triggers (a deletion moves the badge and notifies nobody), so sharing a
	// consumer would couple one's redelivery to the other's failures. Its filter
	// is the whole message.* family, as the indexer's is, because both a
	// creation and a deletion move the badge.
	unreadFanout := channel.NewUnreadFanout(channel.NewRepository(pool), natsClient, l)
	bind(durableSpec{durable: "unread-fanout", filter: "superops.*.message.*", handle: unreadFanout.HandleMessage})

	// ---------------------------------------------------------------------
	// Workflows
	// ---------------------------------------------------------------------
	//
	// BOUND UNCONDITIONALLY, like the mailer and unlike anything lazy. The
	// stream is InterestPolicy: a message is retained only while some consumer
	// has interest in its subject, so a trigger consumer that appeared only
	// once the first workflow was saved would miss every event published before
	// that moment — including the ones that happened while somebody was in the
	// middle of writing the workflow.
	//
	// The filter is an explicit ALLOWLIST rather than superops.>, for the same
	// reason: a wildcard would create interest in every subject and start
	// persisting presence and typing traffic to disk.
	workflowRepo := workflow.NewRepository(pool)
	bind(durableSpec{
		durable: "workflow-trigger",
		filters: workflow.TriggerSubjects,
		handle:  workflow.NewTriggerConsumer(workflowRepo, l).Handle,
	})

	// Outbound mail. The API renders a message and publishes it durably; this
	// consumer is what actually talks to a relay, so a provider outage becomes
	// redelivery here instead of a failed HTTP request there.
	//
	// The transport is built from the same config helper the API uses, so the
	// admin configuration test cannot verify one transport while a different one
	// sends. A failure is fatal for the same reason the stream is: a worker that
	// looks healthy while silently sending no invitations is worse than one that
	// refuses to start.
	mailSender, err := app.NewMailSender(cfg, l)
	if err != nil {
		l.Error("mail transport unavailable; refusing to start with a queue nothing will drain", "error", err)
		return 1
	}
	bind(durableSpec{
		durable: mail.ConsumerDurable,
		filter:  mail.ConsumerFilter,
		handle:  mail.NewConsumer(mailSender, l).HandleRequest,
	})
	l.Info("outbound mail consumer ready", "transport", mailSender.Name())

	// The SHARED INBOX's outbound consumer. A separate durable from the one
	// above, not a widened filter: the subject has five tokens where the
	// transactional one has four, and a NATS '*' matches exactly one token, so
	// neither can see the other's messages. They also carry different payloads
	// — that one queues a rendered message, this one queues a row id and
	// renders from the database, because a reply is durable content an agent
	// can see in the thread.
	mailboxOut := mailbox.NewConsumer(pool, mailSender, l)
	bind(durableSpec{
		durable: mailbox.OutboundDurable,
		filter:  mailbox.OutboundFilter,
		handle:  mailboxOut.Handle,
	})

	// --- periodic jobs -------------------------------------------------------

	var jobs sync.WaitGroup
	start := func(name string, startDelay, interval time.Duration, fn func(context.Context) error) {
		health.register(name, startDelay+3*interval)
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			runLoop(ctx, l, health, name, startDelay, interval, fn)
		}()
	}

	authRepo := auth.NewRepository(pool)
	start(jobSessionCleanup, sessionCleanupInterval, sessionCleanupInterval, func(c context.Context) error {
		return sessionCleanup(c, pool, authRepo, sso.NewRepository(pool), l)
	})

	messageRepo := message.NewRepository(pool)
	start(jobScheduledSend, scheduledInterval, scheduledInterval, func(c context.Context) error {
		return promoteScheduled(c, pool, messageRepo, natsClient, l)
	})

	start(jobRetention, retentionStartDelay, retentionInterval, func(c context.Context) error {
		return runRetention(c, pool, searchSvc, l)
	})

	start(jobACLDrift, aclDriftStartDelay, aclDriftInterval, func(c context.Context) error {
		return runACLDrift(c, pool, az, l)
	})

	// Storage-quota drift. quota.Recompute existed, was documented as "the
	// counterpart to the incremental arithmetic … drift here is a capacity and
	// billing bug", had a passing test proving it restores the invariant — and
	// nothing ever called it. So bytes_used drifted from the truth permanently
	// and invisibly, and the green test is exactly what kept that invisible.
	//
	// Same cadence and same posture as acl_drift: it REPORTS. A quota that
	// disagrees with the files is a number an operator needs to see, not a
	// reason to refuse an upload.
	start(jobQuotaDrift, quotaDriftStartDelay, quotaDriftInterval, func(c context.Context) error {
		ran, err := withSingletonLock(c, pool, lockQuotaDrift, func(c context.Context) error {
			return runQuotaDrift(c, pool, l)
		})
		if err != nil {
			return err
		}
		if !ran {
			l.Debug("quota drift skipped: another replica holds the lock")
		}
		return nil
	})

	// --- unified inbox jobs ---------------------------------------------------

	// The digest renders IN THE WORKER, before queueing. mail.Request carries a
	// fully rendered Message so the mail consumer needs no database access; a
	// digest has to aggregate at send time, which looks like a violation and is
	// not — the digest JOB aggregates and hands the consumer a finished message,
	// so "nothing unrendered goes on the mail queue" still holds.
	mailRenderer, err := app.NewMailRenderer(cfg)
	if err != nil {
		l.Error("mail renderer unavailable; refusing to start with a digest job that cannot render", "error", err)
		return 1
	}
	digester := inbox.NewDigester(inboxRepo, mailRenderer, mail.NewPublisher(natsClient, l), inbox.DigestConfig{
		QuietPeriod: cfg.Inbox.DigestQuietPeriod,
		MinInterval: cfg.Inbox.DigestMinInterval,
	}, l)
	start(jobInboxDigest, inboxDigestStartDelay, inboxDigestInterval, func(c context.Context) error {
		return runInboxDigest(c, pool, digester, l)
	})

	reconciler := inbox.NewReconciler(inboxRepo, l)
	start(jobInboxReconcile, inboxReconcileDelay, inboxReconcileInterval, func(c context.Context) error {
		return runInboxReconcile(c, pool, reconciler, l)
	})

	// --- audit jobs -----------------------------------------------------------

	// A missing partition is a failed INSERT, i.e. a LOST AUDIT RECORD, so this
	// job's failure has to be loud: runLoop records it in /health's last_error,
	// which is the only place an operator would otherwise learn about it.
	start(jobAuditPartition, auditPartitionDelay, auditPartitionInterval, func(c context.Context) error {
		return runAuditPartitions(c, pool, cfg.Audit.RetentionDays, l)
	})

	auditSink, err := audit.NewSink(audit.SinkConfig{
		Transport: cfg.Audit.Sink,
		Path:      cfg.Audit.SinkPath,
		Endpoint:  cfg.Audit.SinkEndpoint,
		Secret:    cfg.Audit.SinkSecret,
		Timeout:   cfg.Audit.SinkTimeout,
		Logger:    l,
	})
	if err != nil {
		// The same §3c rule the API follows: a transport named without its
		// credentials is a boot failure, not a first-use failure.
		l.Error("audit sink unavailable; refusing to start with a chain nothing will anchor", "error", err)
		return 1
	}
	verifier := audit.NewVerifier(pool, auditSink, l)
	start(jobAuditVerify, auditVerifyStartDelay, auditVerifyInterval, func(c context.Context) error {
		return runAuditVerify(c, pool, verifier, l)
	})
	l.Info("audit anchoring ready", "sink", auditSink.Name())

	if store := openStorage(ctx, cfg, l); store != nil {
		// The thumbnailer. Its own durable so a hostile image cannot stall
		// search indexing, and its own subject so it gets the storage key and
		// the content type the indexer has no business knowing.
		thumbs := thumb.NewConsumer(pool, store, l)
		bind(durableSpec{
			durable: "thumbnailer",
			filter:  "superops.*.thumbnail.requested",
			handle:  thumbs.Handle,
		})

		// The trash purge. It is the ONLY thing that clears trashed_at, and
		// internal/file's collector deliberately excludes trashed rows so the
		// two cannot race for the same object.
		driveRepo := drive.NewRepository(pool, az, registry.New())
		// The publisher is what unindexes. A purge destroys the rows, and
		// cmd/reindex only upserts from live ones — so anything this job
		// removes without an event stays in the search index permanently,
		// answering queries for content that no longer exists anywhere.
		drivePub := drive.NewPublisher(natsClient, l)
		start(jobDriveTrash, driveTrashStartDelay, driveTrashInterval, func(c context.Context) error {
			ran, err := withSingletonLock(c, pool, lockDriveTrash, func(c context.Context) error {
				purged, err := drive.Purge(c, driveRepo, store, drive.PurgeOptions{Now: time.Now()}, l)
				if err != nil {
					return err
				}
				drivePub.PublishFileDeletions(c, purged.Removed)
				return nil
			})
			if err != nil {
				return err
			}
			if !ran {
				l.Debug("drive trash purge skipped: another replica holds the lock")
			}
			return nil
		})

		fileRepo := file.NewRepository(pool)
		start(jobObjectGC, objectGCStartDelay, objectGCInterval, func(c context.Context) error {
			return runObjectGC(c, pool, fileRepo, store, l)
		})
	}

	// The step engine. NOT a durable consumer and NOT under a singleton lock:
	// ClaimRun is FOR UPDATE SKIP LOCKED, so every replica drains the same
	// queue and each takes a different row — the shape promoteScheduled uses.
	// A lock here would serialize every workflow in the deployment behind one
	// worker.
	//
	// The actions are constructed with REAL implementations. An executor built
	// without them fails every run at its first step, which is the correct
	// fail-closed reading: a workflow that cannot perform its steps has not
	// succeeded, and reporting green would make the run list a wall of lies.
	workflowExec := workflow.NewExecutor(workflowRepo, l,
		workflow.NewMessageAction(az, message.NewWorkflowPoster(pool, natsClient, l)),
		workflow.NewNotifyProductionAction(az, pool, workflow.NATSInbox{Client: natsClient}),
		workflow.NewCommentAction(az, comment.NewRepository(pool, az)),
	)
	start(jobWorkflowStep, workflowStepStartDelay, workflowStepInterval, func(c context.Context) error {
		return drainWorkflowRuns(c, workflowRepo, workflowExec, l)
	})

	// Projection repair.
	//
	// THE SERVER CANNOT PRODUCE A PROJECTION. It never interprets a CRDT update,
	// so a document whose stored text sits behind its log can only be repaired
	// by a client that has the document in memory. Three paths do that — the
	// editor's debounce, its flush on unmount, and its catch-up on open — and
	// all three need somebody to be there. This is the fourth, for the document
	// that was edited, closed by a browser that was killed before it could
	// flush, and then never opened again: stale in search, with nothing else in
	// the system able to notice.
	//
	// It ASKS rather than fixes. If a room is empty there is nobody to ask, and
	// the log is not growing either — so the staleness is logged and waits for
	// the next person to open the document, which is exactly what the catch-up
	// path is for.
	if natsClient.Conn != nil {
		start(jobProjectionRepair, projectionRepairStartDelay, projectionRepairInterval,
			func(c context.Context) error {
				ran, err := withSingletonLock(c, pool, lockProjectionRepair, func(c context.Context) error {
					return repairProjections(c, pool, natsClient.Conn, l)
				})
				if err != nil {
					return err
				}
				if !ran {
					l.Debug("projection repair skipped: another replica holds the lock")
				}
				return nil
			})
	}

	// The unsent-mail sweep. The publish from the reply handler is best effort
	// by design — an agent's reply must not fail because a message bus was
	// unreachable — so something has to notice what the queue lost. It is also
	// what drains every reply written before the delivery path existed at all.
	start(jobMailSweep, mailSweepStartDelay, mailSweepInterval, func(c context.Context) error {
		ran, err := withSingletonLock(c, pool, lockMailSweep, func(c context.Context) error {
			n, err := mailboxOut.SweepUnsent(c, 200)
			if err != nil {
				return err
			}
			if n > 0 {
				l.Warn("delivered replies the queue had lost", "count", n)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !ran {
			l.Debug("mail sweep skipped: another replica holds the lock")
		}
		return nil
	})

	// The reaper. A worker killed mid-run leaves its row 'running' forever;
	// retrying is safe precisely because of the effects table, so the only
	// thing needed is somebody to notice.
	start(jobWorkflowReaper, workflowReaperDelay, workflowReaperInterval, func(c context.Context) error {
		ran, err := withSingletonLock(c, pool, lockWorkflowReaper, func(c context.Context) error {
			n, err := workflowRepo.ReleaseStaleRuns(c, workflowRunStale)
			if err != nil {
				return err
			}
			if n > 0 {
				l.Warn("released workflow runs whose worker never finished them", "count", n)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !ran {
			l.Debug("workflow reaper skipped: another replica holds the lock")
		}
		return nil
	})

	// The huddle reconciler. It exists because at-least-once webhook delivery
	// prevents duplicates and not LOSSES: a room_finished that never arrived
	// leaves a huddle live forever, and the partial unique index then refuses
	// every new call on that channel. Nothing else would ever notice.
	if cfg.RTC.IsEnabled() {
		media := &rtc.LiveKit{
			Host:      cfg.RTC.Host,
			APIKey:    cfg.RTC.APIKey,
			APISecret: cfg.RTC.APISecret,
			HTTP:      rtc.NewHTTP(cfg.RTC.HTTPTimeout),
		}
		huddleRepo := huddle.NewRepository(pool)
		start(jobHuddleReconcile, huddleReconcileStartDelay, huddleReconcileInterval,
			func(c context.Context) error {
				ran, err := withSingletonLock(c, pool, lockHuddleReconcile, func(c context.Context) error {
					return reconcileHuddles(c, huddleRepo, media, l)
				})
				if err != nil {
					return err
				}
				if !ran {
					l.Debug("huddle reconcile skipped: another replica holds the lock")
				}
				return nil
			})
	}

	// --- health endpoint -----------------------------------------------------

	healthSrv := startHealthServer(l, health)

	quit := make(chan os.Signal, 2)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	l.Info("worker ready, processing events...")
	sig := <-quit
	l.Info("worker shutting down", "signal", sig.String())

	deadline := time.Now().Add(drainTimeout)

	// A second signal abandons the drain rather than being ignored.
	forced := make(chan struct{})
	go func() {
		select {
		case s := <-quit:
			l.Warn("second signal during shutdown, abandoning drain", "signal", s.String())
			close(forced)
		case <-time.After(time.Until(deadline)):
		}
	}()

	if healthSrv != nil {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = healthSrv.Shutdown(shutCtx)
		shutCancel()
	}

	// 1. Stop pulling new events but let buffered ones run to completion.
	for _, cc := range consumers {
		cc.Drain()
	}
	for _, cc := range consumers {
		select {
		case <-cc.Closed():
		case <-forced:
		case <-time.After(time.Until(deadline)):
		}
	}

	// 2. Wait for handler callbacks that are still writing.
	if !waitBounded(&handlers, time.Until(deadline), forced) {
		l.Warn("timed out waiting for in-flight event handlers")
	}

	// 2b. Only now flush the push queue. No handler is still running, so nothing
	//     can enqueue any more, and Close drains what is already there instead of
	//     discarding the pushes for the last events this replica processed.
	if pushDispatcher != nil {
		pushDispatcher.Close()
		if n := pushDispatcher.Dropped(); n > 0 {
			l.Warn("push: notifications dropped by a full queue during this run", "count", n)
		}
	}

	// 3. Only now stop the job loops; a job mid-transaction gets its context
	//    cancelled, which rolls back rather than half-commits.
	cancel()
	if !waitBounded(&jobs, time.Until(deadline), forced) {
		l.Warn("timed out waiting for background jobs")
	}

	// 4. Flush anything the handlers published on their way out.
	if err := natsClient.Drain(); err != nil {
		l.Warn("nats drain incomplete", "error", err)
	}

	l.Info("worker stopped")
	return 0
}

// ensureStreamWithRetry gives NATS a chance to finish coming up before the
// worker gives up on it.
func ensureStreamWithRetry(ctx context.Context, nc *natspkg.Client, l *slog.Logger) error {
	const attempts = 10
	var err error
	for i := 1; i <= attempts; i++ {
		if err = app.EnsureEventStream(ctx, nc, l); err == nil {
			return nil
		}
		if i == attempts {
			break
		}
		l.Warn("JetStream not ready, retrying", "attempt", i, "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// waitBounded reports whether wg finished before the timeout (or the forced
// channel closed).
func waitBounded(wg *sync.WaitGroup, timeout time.Duration, forced <-chan struct{}) bool {
	if timeout <= 0 {
		timeout = time.Second
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-forced:
		return false
	case <-time.After(timeout):
		return false
	}
}

// --- push ---------------------------------------------------------------------

// buildPush constructs the push pipeline, or returns nils when PUSH_ENABLED is
// off (the default).
//
// The two interface values are returned separately from the concrete dispatcher
// on purpose. Assigning a nil *push.Dispatcher to a notification.Pusher yields a
// non-nil interface holding a nil pointer, so the fan-out's `s.pusher == nil`
// guard would sail straight past it and into a nil dereference on the first
// notification. Only the enabled branch ever assigns them.
func buildPush(
	cfg *app.Config,
	pool *pgxpool.Pool,
	l *slog.Logger,
) (inbox.DeviceTokenLister, inbox.Pusher, *push.Dispatcher) {
	if !cfg.Push.IsEnabled() {
		l.Info("push notifications disabled by configuration (PUSH_ENABLED)")
		return nil, nil, nil
	}

	userRepo := user.NewRepository(pool)

	sender := push.NewExpoSender(push.ExpoConfig{
		Endpoint:    cfg.Push.Endpoint,
		AccessToken: cfg.Push.AccessToken,
		Timeout:     cfg.Push.Timeout,
		Logger:      l,
		// A DeviceNotRegistered receipt is the only authoritative signal that a
		// token is dead — the app was uninstalled, or the OS rotated it. Acting
		// on it is not housekeeping: an unremoved dead token is re-sent and
		// re-rejected on every notification that user ever receives, and it
		// never starts working again.
		OnInvalidTokens: func(ctx context.Context, tokens []string) {
			// Fresh deadline. ctx is the batch's send context and may be nearly
			// spent by the very request that produced these receipts, which
			// would leave the dead tokens in place until the next one.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()

			n, err := userRepo.DeleteDeviceTokens(cleanupCtx, tokens)
			if err != nil {
				l.Warn("push: could not delete dead device tokens", "count", len(tokens), "error", err)
				return
			}
			l.Info("push: deleted dead device tokens", "count", n)
		},
	})

	dispatcher := push.NewDispatcher(sender, l, push.DispatcherConfig{
		QueueSize: cfg.Push.QueueSize,
		Workers:   cfg.Push.Workers,
		// Comfortably above one request's own timeout: a dispatcher batch is at
		// most push.MaxBatchSize messages, which is exactly one request.
		SendTimeout: 2 * cfg.Push.Timeout,
	})

	l.Info("push notifications enabled", "provider", "expo",
		"note", "delivery additionally requires APNs/FCM credentials on the Expo project")

	return userRepo, dispatcher, dispatcher
}

// --- durable consumers -------------------------------------------------------

type durableSpec struct {
	durable string
	filter  string
	// filters is an ALLOWLIST of subjects, for a consumer that needs several
	// but not all of them. It exists because the stream is InterestPolicy: a
	// consumer bound to `superops.>` creates interest in every subject, so
	// every presence transition and typing indicator would start being
	// persisted to disk for it. Only one consumer needs this — the workflow
	// trigger — and giving it a wildcard would quietly turn an ephemeral
	// firehose into a durable one.
	//
	// Exactly one of filter and filters is set.
	filters []string
	handle  func(context.Context, *nats.Msg) error
}

// permanentError is implemented by handler errors that redelivery cannot fix:
// a payload that does not parse, a subject with no workspace id, a document
// Meilisearch rejected outright. It is matched structurally so this file does
// not have to import — and enumerate — every domain package's error type.
type permanentError interface{ Permanent() bool }

func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p) && p.Permanent()
}

// nakDelay is the redelivery delay to request for the delivery that just
// failed, mirroring the consumer's own backoff schedule.
func nakDelay(delivery int) time.Duration {
	i := delivery - 1
	if i < 0 {
		i = 0
	}
	if i >= len(consumerBackOff) {
		i = len(consumerBackOff) - 1
	}
	return consumerBackOff[i]
}

// termReason renders a TERM advisory: single line, bounded length.
func termReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > termReasonMax {
		return s[:termReasonMax] + "..."
	}
	return s
}

// bindDurable creates (or updates) a durable pull consumer and starts consuming.
//
// The ack decision is the whole point of the exercise:
//
//   - handler returns nil          → Ack. The work is done.
//   - handler returns a permanent  → Term, with a reason, and the drop is
//     error                          recorded in /health. Redelivering a
//     message that can never succeed is its own outage.
//   - handler returns anything     → Nak with the backoff delay, up to
//     else                           MaxDeliver, then Term.
//
// Before this, the handlers were `func(*nats.Msg)`: they logged their failures
// internally and returned nothing, so a Meilisearch write failure or a Postgres
// blip acked exactly like a success and the event was gone — and nothing in
// this system reconciles the search index or missed notifications after the
// fact. Only a panic was distinguishable from success.
func bindDurable(
	ctx context.Context,
	nc *natspkg.Client,
	l *slog.Logger,
	handlers *sync.WaitGroup,
	health *healthState,
	spec durableSpec,
) (jetstream.ConsumeContext, error) {
	stream, err := nc.JetStream.Stream(ctx, app.EventStreamName)
	if err != nil {
		return nil, fmt.Errorf("open stream %s: %w", app.EventStreamName, err)
	}

	cfg := jetstream.ConsumerConfig{
		Durable:       spec.durable,
		FilterSubject: spec.filter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       consumerAckWait,
		MaxDeliver:    consumerMaxDeliver,
		BackOff:       consumerBackOff,
		MaxAckPending: consumerMaxPending,
	}
	if len(spec.filters) > 0 {
		// FilterSubject and FilterSubjects are mutually exclusive in the
		// JetStream API; setting both is an error rather than a union.
		cfg.FilterSubject = ""
		cfg.FilterSubjects = spec.filters
	}

	consumer, err := stream.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create consumer %s: %w", spec.durable, err)
	}

	return consumer.Consume(func(msg jetstream.Msg) {
		handlers.Add(1)
		defer handlers.Done()

		delivery := 1
		if md, err := msg.Metadata(); err == nil && md.NumDelivered > 0 {
			delivery = int(md.NumDelivered)
		}

		hctx, cancel := context.WithTimeout(ctx, handlerTimeout)
		defer cancel()

		err := invokeHandler(hctx, l, spec, msg)

		switch {
		case err == nil:
			if ackErr := msg.Ack(); ackErr != nil {
				// The work is done but the ack was lost: the server will redeliver
				// and the handler will redo it. Every handler behind this is
				// idempotent, which is what makes that survivable.
				l.Warn("ack failed", "durable", spec.durable, "subject", msg.Subject(), "error", ackErr)
				return
			}
			health.tick(spec.durable)

		case isPermanent(err):
			l.Error("event dropped: permanently unprocessable",
				"durable", spec.durable, "subject", msg.Subject(), "delivery", delivery, "error", err)
			if termErr := msg.TermWithReason(termReason(err.Error())); termErr != nil {
				l.Warn("term failed", "durable", spec.durable, "subject", msg.Subject(), "error", termErr)
			}
			health.fail(spec.durable, fmt.Errorf("terminated %s: %w", msg.Subject(), err))

		case delivery >= consumerMaxDeliver:
			l.Error("event dropped after final delivery",
				"durable", spec.durable, "subject", msg.Subject(), "delivery", delivery, "error", err)
			if termErr := msg.TermWithReason(termReason("failed on every delivery: " + err.Error())); termErr != nil {
				l.Warn("term failed", "durable", spec.durable, "subject", msg.Subject(), "error", termErr)
			}
			health.fail(spec.durable, fmt.Errorf("dropped %s after %d failed deliveries: %w", msg.Subject(), delivery, err))

		default:
			delay := nakDelay(delivery)
			l.Warn("event handler failed, redelivering",
				"durable", spec.durable, "subject", msg.Subject(),
				"delivery", delivery, "retry_in", delay.String(), "error", err)
			if nakErr := msg.NakWithDelay(delay); nakErr != nil {
				l.Warn("nak failed", "durable", spec.durable, "subject", msg.Subject(), "error", nakErr)
			}
		}
	})
}

// invokeHandler runs one handler, converting a panic into an ordinary (and
// retryable) error so a single bad payload cannot take the consumer down.
func invokeHandler(ctx context.Context, l *slog.Logger, spec durableSpec, msg jetstream.Msg) (err error) {
	defer func() {
		if p := recover(); p != nil {
			l.Error("event handler panicked",
				"durable", spec.durable, "subject", msg.Subject(), "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()

	// The JetStream metadata is carried across as a HEADER because the
	// conversion to *nats.Msg drops it: nats.Msg.Metadata() parses the reply
	// subject, and this message has none, so a handler calling it gets an
	// error rather than the stream sequence.
	//
	// The workflow trigger consumer needs that sequence as its idempotency key.
	// It is the only thing that identifies a message stably across
	// redeliveries — a generated id would differ on every retry, and every
	// retry would start another run of the same workflow. Losing it silently
	// made the consumer refuse to enqueue anything at all, which is how this
	// was found: the worker booted, replayed the stream, and logged the refusal
	// once per event.
	header := nats.Header(msg.Headers())
	if header == nil {
		header = nats.Header{}
	}
	if md, mdErr := msg.Metadata(); mdErr == nil && md != nil {
		header.Set(natspkg.HeaderStreamSequence, strconv.FormatUint(md.Sequence.Stream, 10))
	}

	return spec.handle(ctx, &nats.Msg{
		Subject: msg.Subject(),
		Data:    msg.Data(),
		Header:  header,
	})
}

// --- job loop scaffolding -----------------------------------------------------

func runLoop(
	ctx context.Context,
	l *slog.Logger,
	health *healthState,
	name string,
	startDelay, interval time.Duration,
	fn func(context.Context) error,
) {
	timer := time.NewTimer(startDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		health.attempt(name)
		if err := fn(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			health.fail(name, err)
			l.Error("job failed", "job", name, "error", err)
		} else {
			health.tick(name)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// repairProjections asks the rooms of stale documents to re-project.
//
// The query is the whole design. It compares the collaboration log's head
// against the stored projection's seq, and the LEFT JOIN matters: a document
// that has never been projected has no row at all, and an inner join would hide
// exactly the documents that are most wrong.
func repairProjections(ctx context.Context, pool *pgxpool.Pool, nc *nats.Conn, l *slog.Logger) error {
	found, err := collab.FindStaleProjections(ctx, pool,
		projectionRepairGap, projectionRepairAge, projectionRepairBatch)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return nil
	}

	// EVERY DOCUMENT IN THE BATCH IS ASKED, even if one publish fails.
	//
	// FindStaleProjections CLAIMS the batch — it stamps repair_requested_at in
	// the same statement, which is what stops an unanswerable document holding
	// the queue. Returning on the first publish error therefore left documents
	// k..N stamped "just asked", asked nothing, and sent to the back of the
	// queue behind everything else, deferred by a full cycle. One NATS hiccup
	// silently skipped up to a hundred documents, and the WARN below — the
	// operator's only signal — was never reached on that path.
	asked, failed := 0, 0
	for _, s := range found {
		// Published with no origin: this process has no hub, so there is no
		// local delivery to suppress.
		if err := ws.PublishRoomLeaderRequest(nc, "", s.DocumentID, ws.TypeCollabProject, map[string]any{
			"document_id":    s.DocumentID,
			"head_seq":       s.HeadSeq,
			"projection_seq": s.ProjectionSeq,
		}); err != nil {
			failed++
			l.Error("could not request a projection repair",
				"document_id", s.DocumentID, "error", err)
			continue
		}
		asked++
	}

	// WARN, not Debug. Every one of these is a document that is wrong in search
	// right now, and if the number does not fall over successive sweeps then the
	// rooms are empty and the requests are going nowhere — which an operator
	// needs to be able to see without turning on debug logging.
	l.Warn("asked rooms to repair stale projections",
		"documents", asked, "failed", failed, "oldest_gap", found[0].Gap())
	if failed > 0 {
		// Reported to the health registry too: a sweep that could not ask is a
		// sweep whose claim was spent for nothing, and the next one will not
		// revisit those documents until every other stale one has had a turn.
		return fmt.Errorf("projection repair: %d of %d requests failed", failed, len(found))
	}
	return nil
}

// runQuotaDrift re-derives every workspace's storage usage and logs what moved.
//
// Reports rather than blocks, for the same reason runACLDrift does: a quota that
// disagrees with the files is a capacity and billing bug, and refusing uploads
// over it would turn an accounting error into an outage.
func runQuotaDrift(ctx context.Context, pool *pgxpool.Pool, l *slog.Logger) error {
	rows, err := pool.Query(ctx, `SELECT id::text FROM workspaces ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list workspaces for quota drift: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan workspace: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	drifted := 0
	for _, id := range ids {
		before, after, err := quota.Recompute(ctx, pool, id)
		if err != nil {
			return err
		}
		if before == after {
			continue
		}
		drifted++
		// One line per drifting workspace, capped, so a systemic problem is
		// visible without filling a disk when a backfill has not been run.
		if drifted <= aclDriftSamples {
			l.Warn("storage usage drifted from the files that back it",
				"workspace_id", id, "recorded", before, "actual", after,
				"delta", after-before)
		}
	}
	if drifted > 0 {
		l.Warn("storage quota drift corrected", "workspaces", drifted, "of", len(ids))
	}
	return nil
}

// withSingletonLock runs fn while holding a session-scoped Postgres advisory
// lock, so exactly one replica executes the job for this tick. It reports
// whether fn ran at all.
//
// Without it every replica ran the retention DELETE concurrently, immediately
// at startup — N replicas competing for the same rows during the exact window
// a deploy is already stressing the database.
func withSingletonLock(ctx context.Context, pool *pgxpool.Pool, key int64, fn func(context.Context) error) (bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	if !acquired {
		return false, nil
	}
	defer func() {
		// Release on a fresh context: the caller's may already be cancelled by
		// shutdown, and a lock leaked until the connection dies blocks the next
		// tick on every replica.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			// The connection is discarded on release if it is broken, which
			// drops the lock anyway.
			_ = err
		}
	}()

	return true, fn(ctx)
}

// --- session cleanup ----------------------------------------------------------

// sessionCleanup deletes expired sessions and marks long-expired invitations.
//
// It calls auth.Repository.CleanExpiredSessions rather than inlining the DELETE:
// the repository owns the sessions table, and the inline copy also discarded its
// error, so a failing cleanup was indistinguishable from a clean one.
// sessionCleanup expires everything that was started and never finished.
//
// It carries the SSO half too, because both of those functions documented "the
// background worker runs this on a timer" while nothing called either. The
// tables grew without bound, holding abandoned sign-in state — including
// pending logins — indefinitely. They share this job rather than getting their
// own because they share its reason, its cadence and its lock; two jobs on one
// advisory key would silently serialise, and a second key for three DELETEs
// that always run together is bookkeeping with no benefit.
func sessionCleanup(ctx context.Context, pool *pgxpool.Pool, repo *auth.Repository,
	ssoRepo *sso.Repository, l *slog.Logger) error {
	ran, err := withSingletonLock(ctx, pool, lockSessionCleanup, func(ctx context.Context) error {
		n, err := repo.CleanExpiredSessions(ctx)
		if err != nil {
			return fmt.Errorf("clean expired sessions: %w", err)
		}
		if n > 0 {
			l.Info("cleaned expired sessions", "count", n)
		}

		tag, err := pool.Exec(ctx,
			`UPDATE invitations SET status = 'expired' WHERE status = 'pending' AND expires_at < NOW()`)
		if err != nil {
			return fmt.Errorf("expire invitations: %w", err)
		}
		if tag.RowsAffected() > 0 {
			l.Info("expired stale invitations", "count", tag.RowsAffected())
		}

		if ssoRepo != nil {
			n, err := ssoRepo.CleanExpiredAuthRequests(ctx)
			if err != nil {
				return err
			}
			if n > 0 {
				l.Info("cleaned expired SSO auth requests", "count", n)
			}
			n, err = ssoRepo.CleanExpiredPendingLogins(ctx)
			if err != nil {
				return err
			}
			if n > 0 {
				l.Info("cleaned expired SSO pending logins", "count", n)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !ran {
		l.Debug("session cleanup skipped: another replica holds the lock")
	}
	return nil
}

// --- scheduled messages -------------------------------------------------------

type promoted struct {
	id        string
	channelID string
}

// promoteScheduled turns due scheduled messages into live ones.
//
// Three things it now gets right:
//   - Each promoted row gets a distinct created_at. A single
//     `SET created_at = NOW()` gave every message in the tick the same
//     timestamp, so their relative order was whatever the index felt like.
//   - It publishes the same hydrated message the REST path publishes, rather
//     than a hand-built subset with no reactions, files, is_edited or
//     reply_count — subscribers could tell a scheduled message from a normal
//     one by its missing fields.
//   - It refuses to promote into an archived channel, matching the 409
//     CHANNEL_ARCHIVED that message.Handler.Send returns.
func promoteScheduled(
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *message.Repository,
	nc *natspkg.Client,
	l *slog.Logger,
) error {
	var due []promoted

	err := database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH locked AS (
			    SELECT m.id, m.scheduled_at
			      FROM messages m
			      JOIN channels c ON c.id = m.channel_id
			     WHERE m.is_scheduled = TRUE
			       AND m.is_deleted = FALSE
			       AND m.scheduled_at IS NOT NULL
			       AND m.scheduled_at <= NOW()
			       AND c.is_archived = FALSE
			     ORDER BY m.scheduled_at, m.id
			     LIMIT $1
			     FOR UPDATE OF m SKIP LOCKED
			), numbered AS (
			    SELECT id, row_number() OVER (ORDER BY scheduled_at, id) AS rn FROM locked
			)
			UPDATE messages m
			   SET is_scheduled = FALSE,
			       created_at   = NOW() + ((numbered.rn - 1) * INTERVAL '1 microsecond'),
			       updated_at   = NOW()
			  FROM numbered
			 WHERE m.id = numbered.id
			RETURNING m.id, m.channel_id`, scheduledPageSize)
		if err != nil {
			return fmt.Errorf("promote scheduled messages: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var p promoted
			if err := rows.Scan(&p.id, &p.channelID); err != nil {
				return fmt.Errorf("scan promoted message: %w", err)
			}
			due = append(due, p)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("promote scheduled messages: %w", err)
		}
		if len(due) == 0 {
			return nil
		}

		ids := make([]string, len(due))
		for i, p := range due {
			ids[i] = p.id
		}

		if _, err := tx.Exec(ctx, `
			UPDATE channels c
			   SET last_message_at = GREATEST(COALESCE(c.last_message_at, 'epoch'::timestamptz), sub.newest)
			  FROM (SELECT channel_id, MAX(created_at) AS newest FROM messages WHERE id = ANY($1) GROUP BY channel_id) sub
			 WHERE c.id = sub.channel_id`, ids); err != nil {
			return fmt.Errorf("bump channel activity: %w", err)
		}

		// Thread replies must bump the parent's reply_count; the live Create
		// path does this and the scheduled path used to bypass it.
		if _, err := tx.Exec(ctx, `
			UPDATE messages p
			   SET reply_count = p.reply_count + sub.n
			  FROM (
			      SELECT parent_id, COUNT(*) AS n
			        FROM messages
			       WHERE id = ANY($1) AND parent_id IS NOT NULL
			       GROUP BY parent_id
			  ) sub
			 WHERE p.id = sub.parent_id`, ids); err != nil {
			return fmt.Errorf("bump reply counts: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}

	for _, p := range due {
		publishPromoted(ctx, pool, repo, nc, l, p)
	}
	l.Info("promoted scheduled messages", "count", len(due))
	return nil
}

// publishPromoted emits the message.created event for one promoted row, with
// the same hydrated payload the REST handler publishes.
func publishPromoted(
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *message.Repository,
	nc *natspkg.Client,
	l *slog.Logger,
	p promoted,
) {
	var workspaceID string
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM channels WHERE id = $1`, p.channelID).Scan(&workspaceID); err != nil {
		l.Error("scheduled: resolve workspace", "channel_id", p.channelID, "error", err)
		return
	}

	msg, err := repo.GetByID(ctx, p.id)
	if err != nil {
		l.Error("scheduled: reload message", "message_id", p.id, "error", err)
		return
	}
	if msg == nil {
		return // deleted between the UPDATE and here
	}
	if err := repo.Hydrate(ctx, []*message.Message{msg}); err != nil {
		l.Error("scheduled: hydrate message", "message_id", p.id, "error", err)
		return
	}

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := nc.PublishDurable(pubCtx,
		"superops."+workspaceID+".message.created",
		"message.new:"+msg.ID,
		natspkg.Event{Type: "message.new", Data: msg},
	); err != nil {
		l.Error("scheduled: publish message", "message_id", p.id, "error", err)
	}

	// AND ITS ATTACHMENTS, which nothing re-indexed.
	//
	// CreateScheduled already called linkFiles, so the file's message_id is set
	// and its ACL has been re-materialized onto the channel — but the send path
	// hands respondWithMessage an empty workspace id (nothing is broadcast for a
	// message that has not been sent yet), so the re-index loop there never
	// ran, and this function only ever emitted message.created. The attachment
	// kept its upload-time index state: readable by its uploader alone, with no
	// channel, so nobody in the channel could find it and `?channel=` never
	// matched. An audit found it by scheduling a message and watching zero file
	// events cross the wire.
	for _, f := range msg.Files {
		// ITS OWN BUDGET, not a slice of the message's. One 3-second context
		// shared across message.created plus N attachment events means the
		// later ones expire on a slow NATS — which is the silent non-indexing
		// this loop exists to fix, arriving through the back door. The REST
		// path gives every publish its own publishTimeout for the same reason.
		fileCtx, fileCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		if err := nc.PublishDurable(fileCtx,
			"superops."+workspaceID+".file.updated",
			"file.updated:"+f.ID+":"+msg.ID,
			natspkg.Event{Type: "file.updated", Data: map[string]any{
				"id":         f.ID,
				"channel_id": msg.ChannelID,
				"file_type":  f.FileType,
				"user_id":    msg.UserID,
				"name":       f.Name,
				"created_at": msg.CreatedAt.UTC().Format(time.RFC3339Nano),
			}},
		); err != nil {
			l.Error("scheduled: publish attachment re-index",
				"message_id", p.id, "file_id", f.ID, "error", err)
		}
		fileCancel()
	}
}

// --- retention ----------------------------------------------------------------

// runRetention enforces per-workspace message retention.
//
// The previous implementation was a single unbounded DELETE, run by every
// replica at startup, that published nothing. It therefore (a) held locks over
// an arbitrarily large row set, (b) left every purged message searchable
// forever, (c) promoted surviving thread replies to top-level messages via
// `parent_id ON DELETE SET NULL`, and (d) never decremented reply_count.
func runRetention(ctx context.Context, pool *pgxpool.Pool, searchSvc *search.Service, l *slog.Logger) error {
	ran, err := withSingletonLock(ctx, pool, lockRetention, func(ctx context.Context) error {
		// unindex runs inside the purge transaction, before the rows go. Doing it
		// after the commit — as this used to — means a Meilisearch hiccup leaves
		// content that retention has permanently destroyed fully readable through
		// GET /api/v1/search, with nothing left in Postgres to retry from. Ordered
		// this way the failure mode inverts: the batch rolls back and is retried
		// next hour, and the worst case is a document missing from the index for
		// a message that still exists, which cmd/reindex can repair.
		unindex := func(ctx context.Context, ids []string) error {
			if searchSvc == nil {
				return nil
			}
			if err := searchSvc.DeleteMessages(ctx, ids); err != nil {
				return fmt.Errorf("retention: remove from search index: %w", err)
			}
			return nil
		}

		total := 0
		for batch := 0; batch < retentionMaxBatches; batch++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			expired, deleted, files, err := purgeRetentionBatch(ctx, pool, retentionBatchSize, unindex)
			if err != nil {
				return err
			}
			if deleted == 0 {
				break
			}
			total += deleted

			if files > 0 {
				// files.message_id is ON DELETE SET NULL, so the rows survive as
				// orphans and the object GC job collects them after its grace
				// period. Log it so the two jobs are correlatable.
				l.Info("retention: files detached, awaiting object GC", "count", files)
			}

			// Measured on the roots the batch query returned, not on the total
			// including thread replies: one big thread would otherwise look like a
			// full page and keep the loop going for no reason.
			if expired < retentionBatchSize {
				break
			}
		}
		if total > 0 {
			l.Info("retention: purged messages", "count", total)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !ran {
		l.Debug("retention skipped: another replica holds the lock")
	}
	return nil
}

// purgeRetentionBatch deletes up to limit expired messages (plus the replies of
// any expired thread root). beforeDelete runs inside the transaction with the
// full set of doomed ids; if it fails, nothing is purged.
//
// It reports how many expired roots the batch query found (the loop control
// signal), how many rows were actually deleted, and how many file rows were
// detached.
func purgeRetentionBatch(
	ctx context.Context,
	pool *pgxpool.Pool,
	limit int,
	beforeDelete func(context.Context, []string) error,
) (expiredCount, deleted int, files int64, err error) {
	err = database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		expired, err := scanIDs(ctx, tx, `
			SELECT m.id
			  FROM messages m
			  JOIN channels c   ON c.id = m.channel_id
			  JOIN workspaces w ON w.id = c.workspace_id
			 WHERE w.retention_days > 0
			   AND m.created_at < NOW() - make_interval(days => w.retention_days)
			 ORDER BY m.created_at
			 LIMIT $1
			 FOR UPDATE OF m SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("select expired messages: %w", err)
		}
		expiredCount = len(expired)
		if expiredCount == 0 {
			return nil
		}

		// A thread is retained or purged as a unit, keyed on the root's age.
		// Deleting only the root would leave its replies with parent_id NULL —
		// i.e. silently promoted into the channel timeline as new-looking
		// top-level messages, which is worse than either keeping or deleting
		// them.
		//
		// Deliberately not SKIP LOCKED here, unlike the root query. Skipping a
		// concurrently-locked reply would delete the root anyway and reintroduce
		// exactly the orphan-promotion this is here to prevent; blocking instead
		// either gets the whole thread or trips the pool's lock_timeout and rolls
		// the batch back for the next tick.
		replies, err := scanIDs(ctx, tx,
			`SELECT id FROM messages WHERE parent_id = ANY($1) FOR UPDATE`, expired)
		if err != nil {
			return fmt.Errorf("select thread replies: %w", err)
		}

		targets := union(expired, replies)

		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM files WHERE message_id = ANY($1)`, targets).Scan(&files); err != nil {
			return fmt.Errorf("count attached files: %w", err)
		}

		// Decrement reply_count on parents that are NOT themselves being purged.
		if _, err := tx.Exec(ctx, `
			UPDATE messages p
			   SET reply_count = GREATEST(p.reply_count - sub.n, 0)
			  FROM (
			      SELECT parent_id, COUNT(*) AS n
			        FROM messages
			       WHERE id = ANY($1)
			         AND parent_id IS NOT NULL
			         AND NOT (parent_id = ANY($1))
			       GROUP BY parent_id
			  ) sub
			 WHERE p.id = sub.parent_id`, targets); err != nil {
			return fmt.Errorf("decrement reply counts: %w", err)
		}

		if beforeDelete != nil {
			if err := beforeDelete(ctx, targets); err != nil {
				return err
			}
		}

		tag, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = ANY($1)`, targets)
		if err != nil {
			return fmt.Errorf("delete expired messages: %w", err)
		}

		deleted = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return expiredCount, deleted, files, nil
}

func scanIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// --- acl drift ------------------------------------------------------------------

// runACLDrift reconciles the derived half of the object-permission
// materialization, then recomputes the whole of it and reports where it still
// disagrees with its definition. See docs/plans/00-permissions.md and
// internal/authz/rebuild.go.
//
// It used to verify only, on the principle that a job which silently repaired
// would take the evidence away with the symptom. That principle is right and is
// still enforced below — but it assumed something was MAINTAINING the derived
// rows, and nothing is. workspaces, channels and files get their acl_object
// rows from acl_object_expected, and no handler writes one at creation time, so
// verify-only reported every object created since the backfill as
// missing_object, for ever, growing. A watchdog that is permanently red is a
// watchdog nobody reads, and it is the only watchdog on denormalized
// authorization state.
//
// So the job reconciles first and says exactly what it changed. That is not a
// silent repair: RebuildStats is logged whenever it is non-zero, so "how much
// was unmaintained" is a number somebody can watch and alert on, and Verify
// still runs afterwards over BOTH halves. Anything it reports now is drift the
// reconcile could not explain — an ACL-native object with the wrong keys, a row
// whose source is gone — which is the class that was always worth finding.
//
// Reconciling here does NOT make acl_key fresh enough to authorize from for the
// derived types: this runs hourly, and a key set that is an hour stale would
// WIDEN a caller's key set, which for a tenancy filter is a leak. That is why
// authz.KeysFor still answers its channel arm from channel_members rather than
// from acl_key — see the comment on KeysFor.
//
// The remaining drift is surfaced twice: as an ERROR log with named examples,
// and as this function's return value, which runLoop records in /health's
// last_error for the job. It does not fail the readiness probe — restarting the
// worker repairs nothing here — but it is impossible to miss in either place.
func runACLDrift(ctx context.Context, pool *pgxpool.Pool, az *authz.Checker, l *slog.Logger) error {
	var stats authz.RebuildStats
	var report authz.DriftReport

	ran, err := withSingletonLock(ctx, pool, lockACLDrift, func(ctx context.Context) error {
		var err error
		if stats, err = az.Rebuild(ctx); err != nil {
			return err
		}
		report, err = az.Verify(ctx, aclDriftSamples)
		return err
	})
	if err != nil {
		return fmt.Errorf("verify acl materialization: %w", err)
	}
	if !ran {
		l.Debug("acl drift check skipped: another replica holds the lock")
		return nil
	}
	if !stats.Clean() {
		// Expected and non-zero on every tick that saw a workspace, channel or
		// file created: those rows are derived and nothing writes them at
		// creation time. Worth logging at Info rather than swallowing, because a
		// sudden jump is how a broken write path shows up here.
		l.Info("acl materialization reconciled",
			"objects_upserted", stats.ObjectsUpserted,
			"objects_deleted", stats.ObjectsDeleted,
			"keys_inserted", stats.KeysInserted,
			"keys_deleted", stats.KeysDeleted)
	}
	if report.Clean() {
		l.Debug("acl drift check clean")
		return nil
	}

	l.Error("acl materialization has drifted and the reconcile did not explain it",
		"missing_keys", report.MissingKeys,
		"extra_keys", report.ExtraKeys,
		"missing_objects", report.MissingObjects,
		"misplaced_objects", report.MisplacedObjects,
		"extra_objects", report.ExtraObjects)
	for _, s := range report.Samples {
		l.Error("acl drift sample",
			"kind", s.Kind, "object_type", s.ObjectType, "object_id", s.ObjectID, "detail", s.Detail)
	}
	return errors.New(report.String())
}

// --- unified inbox ---------------------------------------------------------------

// runInboxDigest batches unread items into one email per (user, workspace).
//
// Advisory-locked so one replica runs it: two replicas selecting the same
// candidates would both claim and both send, which is the exact failure the
// claim-then-send ordering inside Digester.Run exists to make impossible for a
// single runner.
func runInboxDigest(ctx context.Context, pool *pgxpool.Pool, d *inbox.Digester, l *slog.Logger) error {
	var sent int
	ran, err := withSingletonLock(ctx, pool, lockInboxDigest, func(ctx context.Context) error {
		var err error
		sent, err = d.Run(ctx)
		return err
	})
	if err != nil {
		return err
	}
	if !ran {
		l.Debug("inbox digest skipped: another replica holds the lock")
		return nil
	}
	if sent > 0 {
		l.Info("inbox digest: queued", "messages", sent)
	}
	return nil
}

// runInboxReconcile recomputes a rotating slice of users' inbox counters from
// inbox_events, repairs what disagrees and reports it.
//
// It returns the drift as an error so runLoop records it in /health's
// last_error, exactly as runACLDrift does — and, exactly as runACLDrift does, it
// deliberately does NOT fail the readiness probe. Restarting the worker repairs
// nothing here, and a counter that was wrong for fifteen minutes is a wrong
// answer to a question the user asked, not an outage.
func runInboxReconcile(ctx context.Context, pool *pgxpool.Pool, r *inbox.Reconciler, l *slog.Logger) error {
	var reconcileErr error
	ran, err := withSingletonLock(ctx, pool, lockInboxReconcile, func(ctx context.Context) error {
		reconcileErr = r.Run(ctx)
		return nil
	})
	if err != nil {
		return err
	}
	if !ran {
		l.Debug("inbox reconcile skipped: another replica holds the lock")
		return nil
	}
	return reconcileErr
}

// --- audit -----------------------------------------------------------------------

// runAuditPartitions keeps the monthly partition window ahead of NOW() and drops
// the ones that have aged out of AUDIT_RETENTION_DAYS.
//
// This is where audit retention differs from message retention, and the
// difference is the whole point of migration 021. runRetention above is batched,
// capped and locked because an unbounded DELETE on a large table was a
// production problem. Here retention is a DROP TABLE: milliseconds, no locks
// held over user rows, disk returned immediately.
func runAuditPartitions(ctx context.Context, pool *pgxpool.Pool, retentionDays int, l *slog.Logger) error {
	ran, err := withSingletonLock(ctx, pool, lockAuditPartition, func(ctx context.Context) error {
		return audit.RunPartitions(ctx, pool, retentionDays, l)
	})
	if err != nil {
		return fmt.Errorf("audit partitions: %w", err)
	}
	if !ran {
		l.Debug("audit partition maintenance skipped: another replica holds the lock")
	}
	return nil
}

// runAuditVerify walks each workspace's hash chain and ships the head of every
// clean one off-box.
//
// A break is reported here, logged at ERROR by the verifier, and surfaced on
// /health through this function's return value. It is deliberately never a 500
// on a user-facing route: a corrupted audit log must not be a denial of service,
// or corrupting it becomes an attack rather than something an attack leaves
// behind.
func runAuditVerify(ctx context.Context, pool *pgxpool.Pool, v *audit.Verifier, l *slog.Logger) error {
	var statuses []audit.ChainStatus
	var anchorErr error
	ran, err := withSingletonLock(ctx, pool, lockAuditVerify, func(ctx context.Context) error {
		statuses, anchorErr = v.Anchor(ctx)
		return nil
	})
	if err != nil {
		return err
	}
	if !ran {
		l.Debug("audit verify skipped: another replica holds the lock")
		return nil
	}
	if anchorErr != nil {
		return anchorErr
	}

	broken := 0
	unanchored := int64(0)
	for _, st := range statuses {
		if !st.OK {
			broken++
		}
		unanchored += st.HeadSeq - st.AnchoredSeq
	}
	if broken > 0 {
		return fmt.Errorf("audit: %d of %d workspace chains failed verification", broken, len(statuses))
	}
	if unanchored > 0 {
		// Not an error: the sink may simply be slower than the write rate. It is
		// the number that says how much of the log is protected by nothing but a
		// chain in the same database an administrator can rewrite.
		l.Info("audit chains verified", "workspaces", len(statuses), "entries_not_yet_anchored", unanchored)
	}
	return nil
}

// --- orphaned object GC --------------------------------------------------------

// objectGCCursor rotates through file.GCPrefixes so successive runs cover the
// whole keyspace. The collector itself lives in internal/file — it owns both
// the key format and the ownership columns, and as an unexported function here
// the predicate that decides whether a user's file is garbage had no test.
var objectGCCursor atomic.Int64

// openStorage returns nil when object storage is not configured or not
// reachable, in which case the collector simply does not start. A worker that
// refused to boot would also stop indexing and stop sending notifications, over
// a job whose entire purpose is to reclaim disk.
func openStorage(ctx context.Context, cfg *app.Config, l *slog.Logger) storage.Backend {
	if !cfg.MinIO.IsEnabled() {
		l.Info("file storage disabled by configuration; object GC not started")
		return nil
	}
	backend, err := storage.Open(ctx, storage.Config{
		Backend:      cfg.MinIO.Backend,
		Endpoint:     cfg.MinIO.Endpoint,
		AccessKey:    cfg.MinIO.AccessKey,
		SecretKey:    cfg.MinIO.SecretKey,
		Bucket:       cfg.MinIO.Bucket,
		UseSSL:       cfg.MinIO.UseSSL,
		Region:       cfg.MinIO.Region,
		PathStyle:    cfg.MinIO.PathStyle,
		CreateBucket: cfg.MinIO.CreateBucket,
	}, l)
	if err != nil {
		l.Error("object storage unavailable; object GC not started", "error", err)
		return nil
	}
	return backend
}

// runObjectGC takes the singleton lock and runs one collection pass.
//
// The lock stays here rather than moving with the collector: it is a
// worker-replica concern, keyed by this file's lock-id registry, and it is the
// one part of the job with nothing to assert about.
func runObjectGC(
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *file.Repository,
	store storage.Backend,
	l *slog.Logger,
) error {
	ran, err := withSingletonLock(ctx, pool, lockObjectGC, func(ctx context.Context) error {
		prefix := file.GCPrefixes[int(objectGCCursor.Add(1)-1)%len(file.GCPrefixes)]
		_, err := file.Collect(ctx, repo, store, file.CollectOptions{
			Now:         time.Now(),
			SweepPrefix: prefix,
		}, l)
		return err
	})
	if err != nil {
		return err
	}
	if !ran {
		l.Debug("object gc skipped: another replica holds the lock")
	}
	return nil
}

// --- health endpoint -----------------------------------------------------------

type jobStatus struct {
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Stale       bool      `json:"stale"`

	budget time.Duration // 0 = event driven, never stale
}

type healthState struct {
	mu      sync.Mutex
	started time.Time
	names   []string
	jobs    map[string]*jobStatus
}

func newHealthState() *healthState {
	return &healthState{started: time.Now(), jobs: map[string]*jobStatus{}}
}

func (h *healthState) register(name string, budget time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.jobs[name]; ok {
		return
	}
	h.names = append(h.names, name)
	h.jobs[name] = &jobStatus{budget: budget}
}

func (h *healthState) attempt(name string) {
	h.set(name, func(s *jobStatus) { s.LastAttempt = time.Now() })
}

func (h *healthState) tick(name string) {
	h.set(name, func(s *jobStatus) {
		now := time.Now()
		s.LastAttempt, s.LastSuccess, s.LastError = now, now, ""
	})
}

func (h *healthState) fail(name string, err error) {
	h.set(name, func(s *jobStatus) {
		s.LastAttempt = time.Now()
		s.LastError = err.Error()
	})
}

func (h *healthState) set(name string, fn func(*jobStatus)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.jobs[name]
	if !ok {
		s = &jobStatus{}
		h.names = append(h.names, name)
		h.jobs[name] = s
	}
	fn(s)
}

// snapshot renders the current state and reports whether the worker is healthy.
//
// Healthy means "every job loop is still turning" — measured on attempts, not
// successes. A job that keeps failing because Postgres is down is reported in
// the body (last_error) but does not fail the probe: restarting the container
// does not fix a dependency outage, it just adds a CrashLoopBackOff to it.
// A loop that has stopped ticking altogether is a real wedge, and that is what
// the probe catches.
func (h *healthState) snapshot() (bool, map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	healthy := true
	jobs := make(map[string]jobStatus, len(h.jobs))
	for _, name := range h.names {
		s := *h.jobs[name]
		if s.budget > 0 {
			since := s.LastAttempt
			if since.IsZero() {
				since = h.started
			}
			s.Stale = now.Sub(since) > s.budget
			if s.Stale {
				healthy = false
			}
		}
		jobs[name] = s
	}

	status := "ok"
	if !healthy {
		status = "stale"
	}
	return healthy, map[string]any{
		"status":         status,
		"uptime_seconds": int(now.Sub(h.started).Seconds()),
		"jobs":           jobs,
	}
}

// startHealthServer exposes GET /health so Kubernetes can probe the worker at
// all. cmd/worker previously started no listener, so the only thing a probe
// could have asserted was that PID 1 existed — which the kubelet already knows.
func startHealthServer(l *slog.Logger, health *healthState) *http.Server {
	port := 8081
	if raw := os.Getenv("WORKER_HEALTH_PORT"); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			l.Error("WORKER_HEALTH_PORT is not a valid port; using default", "value", raw, "default", port)
		} else {
			port = p
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		healthy, body := health.snapshot()
		status := http.StatusOK
		if !healthy {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.Error("worker health server", "error", err)
		}
	}()
	l.Info("worker health endpoint listening", "addr", srv.Addr, "path", "/health")
	return srv
}

// reconcileHuddles makes the database agree with the media server.
//
// The media server is authoritative for presence, so a disagreement is resolved
// in its favour: a room it has forgotten ends the huddle, and a roster it
// reports replaces ours. That is what repairs the state after a lost webhook —
// which at-least-once delivery does not prevent, because it only guarantees a
// message is delivered AT LEAST once if it is delivered at all.
func reconcileHuddles(ctx context.Context, repo *huddle.Repository, media rtc.Provider, l *slog.Logger) error {
	live, err := repo.StaleLive(ctx, huddleReconcileGrace, 200)
	if err != nil {
		return fmt.Errorf("list live huddles: %w", err)
	}
	var ended, repaired int
	for _, h := range live {
		room, err := media.Room(ctx, h.RoomName)
		switch {
		case errors.Is(err, rtc.ErrRoomNotFound):
			// The room is gone and we were never told. End it, or the channel
			// can never start another call.
			if _, err := repo.End(ctx, h.ID, "reconciled"); err != nil {
				l.Error("end a forgotten huddle", "huddle_id", h.ID, "error", err)
				continue
			}
			ended++
			continue
		case err != nil:
			// A media server that is down must NOT end everybody call. Log and
			// leave the row alone; the next pass will find it.
			l.Warn("could not read a room from the media server",
				"huddle_id", h.ID, "room", h.RoomName, "error", err)
			continue
		}

		if len(room.Participants) == 0 {
			if _, err := repo.End(ctx, h.ID, "empty"); err != nil {
				l.Error("end an empty huddle", "huddle_id", h.ID, "error", err)
				continue
			}
			ended++
			continue
		}

		participants := make([]huddle.Participant, 0, len(room.Participants))
		for _, p := range room.Participants {
			participants = append(participants, huddle.Participant{
				HuddleID:        h.ID,
				ParticipantSID:  p.SID,
				UserID:          p.Identity,
				JoinedAt:        p.JoinedAt,
				IsScreenSharing: p.ScreenSharing,
			})
		}
		if err := repo.ReconcileParticipants(ctx, h.ID, participants); err != nil {
			l.Error("reconcile a huddle roster", "huddle_id", h.ID, "error", err)
			continue
		}
		repaired++
	}
	if ended > 0 || repaired > 0 {
		l.Info("huddles reconciled", "ended", ended, "rosters_repaired", repaired, "checked", len(live))
	}
	return nil
}

// drainWorkflowRuns claims and executes pending runs until the queue is empty.
//
// Bounded per tick so a large backlog cannot starve the other job loops on this
// replica — the next tick picks up where this one stopped, and because claiming
// is SKIP LOCKED the other replicas are draining it at the same time.
func drainWorkflowRuns(ctx context.Context, repo *workflow.Repository,
	exec *workflow.Executor, l *slog.Logger) error {

	for i := 0; i < workflowStepBatch; i++ {
		run, steps, err := repo.ClaimRun(ctx)
		if errors.Is(err, workflow.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim workflow run: %w", err)
		}
		// Executor.Run records the failure on the run and returns nil for
		// anything that is the workflow's fault. A non-nil error here is
		// infrastructure, and returning it puts the job loop's last_error into
		// /health where an operator can see it.
		if err := exec.Run(ctx, run, steps); err != nil {
			return fmt.Errorf("execute workflow run %s: %w", run.ID, err)
		}
		l.Debug("workflow run executed", "run_id", run.ID, "workflow_id", run.WorkflowID)
	}
	return nil
}
