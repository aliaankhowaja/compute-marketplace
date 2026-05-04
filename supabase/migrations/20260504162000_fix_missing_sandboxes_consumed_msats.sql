-- Repair migration for environments missing consumed_msats on sandboxes
-- Safe to run multiple times.

alter table public.sandboxes
  add column if not exists consumed_msats bigint not null default 0;

update public.sandboxes
set consumed_msats = 0
where consumed_msats is null;

comment on column public.sandboxes.consumed_msats is
  'Aggregate msats reported by runtime callbacks (optional fast path)';
