-- LNURL-pay and running total for sandboxes

alter table public.agents add column if not exists lightning_address text;

alter table public.agents drop constraint if exists agents_offer_type_check;

alter table public.agents
  add constraint agents_offer_type_check
  check (offer_type in ('bolt12', 'manual_bolt11', 'lnurl_pay'));

alter table public.sandboxes add column if not exists consumed_msats bigint not null default 0;

comment on column public.sandboxes.consumed_msats is 'Aggregate msats reported by runtime callbacks (optional fast path)';
