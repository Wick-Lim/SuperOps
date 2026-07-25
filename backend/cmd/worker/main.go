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
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/message"
	"github.com/Wick-Lim/SuperOps/backend/internal/notification"
	"github.com/Wick-Lim/SuperOps/backend/internal/push"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// Job names, also used as the keys in the /health payload.
const (
	jobSessionCleanup = "session_cleanup"
	jobScheduledSend  = "scheduled_messages"
	jobRetention      = "retention"
	jobObjectGC       = "object_gc"
	jobACLDrift       = "acl_drift"
	jobInboxDigest    = "inbox_digest"
	jobInboxReconcile = "inbox_reconcile"
	jobAuditPartition = "audit_partitions"
	jobAuditVerify    = "audit_verify"
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

	objectGCGrace = 24 * time.Hour // an upload is unattached until its message is sent
	// objectKeyDateSlack is how much a storage key's date can understate the
	// object's real age: a whole day (the key has date granularity) plus the
	// worst-case timezone skew. See olderThan.
	objectKeyDateSlack = 48 * time.Hour
	objectGCRowBatch   = 500
	objectGCKeyBatch   = 1000
	scheduledPageSize  = 200
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
		indexer := search.NewIndexer(searchSvc, l)
		bind(durableSpec{durable: "indexer", filter: "superops.*.message.*", handle: indexer.HandleMessage})
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
		return sessionCleanup(c, pool, authRepo, l)
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

	if storage := openStorage(cfg, l); storage != nil {
		fileRepo := file.NewRepository(pool)
		start(jobObjectGC, objectGCStartDelay, objectGCInterval, func(c context.Context) error {
			return runObjectGC(c, pool, fileRepo, storage, l)
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

	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       spec.durable,
		FilterSubject: spec.filter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       consumerAckWait,
		MaxDeliver:    consumerMaxDeliver,
		BackOff:       consumerBackOff,
		MaxAckPending: consumerMaxPending,
	})
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

	return spec.handle(ctx, &nats.Msg{
		Subject: msg.Subject(),
		Data:    msg.Data(),
		Header:  nats.Header(msg.Headers()),
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
func sessionCleanup(ctx context.Context, pool *pgxpool.Pool, repo *auth.Repository, l *slog.Logger) error {
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

// objectGCPrefixes shard the bucket sweep; objectGCCursor rotates through them
// so successive runs cover the whole keyspace.
var (
	objectGCPrefixes = []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f"}
	objectGCCursor   atomic.Int64
)

func openStorage(cfg *app.Config, l *slog.Logger) *file.Storage {
	if !cfg.MinIO.IsEnabled() {
		l.Info("file storage disabled by configuration; object GC not started")
		return nil
	}
	storage, err := file.NewStorage(file.StorageConfig{
		Endpoint:  cfg.MinIO.Endpoint,
		AccessKey: cfg.MinIO.AccessKey,
		SecretKey: cfg.MinIO.SecretKey,
		Bucket:    cfg.MinIO.Bucket,
		UseSSL:    cfg.MinIO.UseSSL,
	}, l)
	if err != nil {
		l.Warn("MinIO not available; object GC not started", "error", err)
		return nil
	}
	return storage
}

// runObjectGC removes storage objects that nothing points at any more.
//
// Two independent leaks: a files row whose message_id went NULL (message
// deleted, or the upload was never attached), and an object whose row is gone
// entirely (workspace deletion cascades the rows away). The first is visible in
// Postgres; the second is only visible by walking the bucket.
func runObjectGC(
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *file.Repository,
	storage *file.Storage,
	l *slog.Logger,
) error {
	ran, err := withSingletonLock(ctx, pool, lockObjectGC, func(ctx context.Context) error {
		cutoff := time.Now().Add(-objectGCGrace)

		// (i) Unattached rows past the grace period. The grace period matters:
		// a file is unattached from upload until its message is sent.
		orphans, err := repo.ListOrphans(ctx, cutoff, objectGCRowBatch)
		if err != nil {
			return err
		}
		removed := make([]string, 0, len(orphans))
		for _, o := range orphans {
			// A row owns its thumbnail as well as its main object; the row is only
			// dropped once every object it owns is gone, so a partial failure is
			// retried next hour instead of leaking the remainder forever.
			ok := true
			for _, key := range o.Keys() {
				if err := storage.Delete(ctx, key); err != nil {
					l.Warn("object gc: delete object", "key", key, "error", err)
					ok = false
				}
			}
			if ok {
				removed = append(removed, o.ID)
			}
		}
		if n, err := repo.DeleteByIDs(ctx, removed); err != nil {
			return err
		} else if n > 0 {
			l.Info("object gc: removed orphaned files", "count", n)
		}

		// (ii) Objects with no row at all.
		//
		// ListKeys has no continuation token, so an unprefixed listing would
		// return the same first N keys every hour and never reach anything past
		// them. Storage keys start with the workspace uuid, so sweeping one hex
		// prefix per run covers the whole bucket every 16 runs.
		prefix := objectGCPrefixes[int(objectGCCursor.Add(1)-1)%len(objectGCPrefixes)]
		keys, err := storage.ListKeys(ctx, prefix, objectGCKeyBatch)
		if err != nil {
			return err
		}
		// Only consider objects old enough that a concurrent upload cannot be
		// mid-flight between PutObject and its INSERT. The key encodes the
		// upload date (workspace/YYYY/MM/DD/id.ext); anything unparseable is
		// left alone rather than guessed at.
		candidates := make([]string, 0, len(keys))
		for _, k := range keys {
			if olderThan(k, cutoff) {
				candidates = append(candidates, k)
			}
		}
		// Checks thumbnail_key as well as storage_key, so a live thumbnail is
		// never mistaken for an object whose row is gone.
		present, err := repo.StorageKeysPresent(ctx, candidates)
		if err != nil {
			return err
		}
		swept := 0
		for _, k := range candidates {
			if present[k] {
				continue
			}
			if err := storage.Delete(ctx, k); err != nil {
				l.Warn("object gc: delete unreferenced object", "key", k, "error", err)
				continue
			}
			swept++
		}
		if swept > 0 {
			l.Info("object gc: removed unreferenced objects", "count", swept)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !ran {
		l.Debug("object gc skipped: another replica holds the lock")
	}
	return nil
}

// olderThan reports whether an object can be proven, from its key alone, to
// have been uploaded before cutoff.
//
// This is the guard that stops the bucket sweep from deleting an upload that
// has been PUT but whose files row is not committed yet, and it has to be
// conservative in the right direction.
//
// All the key carries is a date, so an object under .../2026/07/25/ may have
// been written at any instant during that day — the youngest it can possibly be
// is the *end* of it, not the start. Comparing the start of the day against the
// cutoff, as this used to, collapsed the 24-hour grace period to nothing:
// an object written at 23:59:59 was eligible for deletion two seconds later,
// because its day started 24 hours and one second before the cutoff.
//
// The extra slack on top covers the zone: file.Handler.Upload formats the key
// with time.Now() in local time while this parses it as UTC, so the day can sit
// up to fourteen hours either side of where it looks. Two days of slack makes
// the guarantee "at least objectGCGrace old" hold for any deployment timezone,
// at the price of a candidate becoming eligible two or three days after upload
// instead of one. For a leak collector that is the right trade.
func olderThan(key string, cutoff time.Time) bool {
	day, ok := objectDay(key)
	if !ok {
		return false
	}
	return day.Add(objectKeyDateSlack).Before(cutoff)
}

// objectDay parses the upload date out of a storage key of the shape
// {workspace_id}/YYYY/MM/DD/{file_id}{ext}, written by file.Handler.Upload.
// The returned time is midnight UTC at the start of that day.
func objectDay(key string) (time.Time, bool) {
	parts := splitN(key, '/', 5)
	if len(parts) < 5 {
		return time.Time{}, false
	}
	day, err := time.Parse("2006/01/02", parts[1]+"/"+parts[2]+"/"+parts[3])
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
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
