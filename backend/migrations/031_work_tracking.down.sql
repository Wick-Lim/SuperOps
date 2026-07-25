-- Reverses 031. LOSSY: it drops every project, issue and label.
--
-- The acl_object rows are deleted EXPLICITLY. acl_object.object_id is
-- polymorphic with no foreign key to projects or issues — it cannot have one —
-- and internal/authz's Rebuild prunes DERIVED types only, by design, so nothing
-- else would ever remove them. A grant on an issue that no longer exists keeps
-- answering the "what could this person reach" audit.
DELETE FROM acl_object WHERE object_type IN ('project', 'issue');

DROP TABLE IF EXISTS issue_activity;
DROP TABLE IF EXISTS issue_relations;
DROP TABLE IF EXISTS issue_labels;
DROP TRIGGER IF EXISTS trg_issues_updated_at ON issues;
DROP TABLE IF EXISTS issues;
DROP TABLE IF EXISTS cycles;
DROP TABLE IF EXISTS labels;
DROP TABLE IF EXISTS issue_states;
DROP TABLE IF EXISTS issue_counters;
DROP TRIGGER IF EXISTS trg_projects_updated_at ON projects;
DROP TABLE IF EXISTS projects;
