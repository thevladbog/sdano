DROP TABLE IF EXISTS report;
DROP TABLE IF EXISTS photo;
DROP TABLE IF EXISTS issue_resolution;
DROP TABLE IF EXISTS issue;
DROP TABLE IF EXISTS work_execution_item;
DROP TABLE IF EXISTS work_execution;
DROP TABLE IF EXISTS work_order;
DROP TABLE IF EXISTS checklist_template_item;
DROP TABLE IF EXISTS checklist_template_version;
DROP TABLE IF EXISTS checklist_template;
DROP TABLE IF EXISTS object;
DROP TABLE IF EXISTS contract;
DROP TABLE IF EXISTS refresh_token;
DROP TABLE IF EXISTS device_token;
DROP TABLE IF EXISTS worker_invite;
DROP TABLE IF EXISTS app_user;
DROP TABLE IF EXISTS ops_audit;
DROP TABLE IF EXISTS tenant;

DROP TYPE IF EXISTS report_status;
DROP TYPE IF EXISTS photo_kind;
DROP TYPE IF EXISTS issue_source;
DROP TYPE IF EXISTS issue_status;
DROP TYPE IF EXISTS work_order_status;
DROP TYPE IF EXISTS user_role;
DROP TYPE IF EXISTS tenant_status;

DROP EXTENSION IF EXISTS citext;
