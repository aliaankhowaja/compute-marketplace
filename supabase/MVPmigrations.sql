-- sockt MVP schema + RLS (from MVPplans/DB.md)
-- Run in Supabase SQL Editor as one script; optionally wrap in BEGIN; ... COMMIT;
-- Requires Supabase Auth (auth.users) for FK and signup trigger.

-- === supabase/migrations/20260503120001_users.sql ===
-- public.users: one row per dashboard user; id matches auth.users.id

create table public.users (
  id uuid not null primary key references auth.users (id) on delete cascade,
  email text not null,
  credit_balance_usd_cents bigint not null default 0
    constraint users_credit_balance_non_negative check (credit_balance_usd_cents >= 0),
  warning_level text not null default 'ok',
  created_at timestamp with time zone not null default now()
);

create unique index users_email_idx on public.users using btree (email);

-- === supabase/migrations/20260503120002_agents_api_keys.sql ===
-- Lightning / L402 principals and hashed API keys for credit path

create table public.agents (
  id uuid not null primary key default gen_random_uuid(),
  jwt_sub uuid not null,
  offer_type text not null
    constraint agents_offer_type_check check (offer_type in ('bolt12', 'manual_bolt11')),
  bolt12_offer text,
  balance_msats bigint not null default 0
    constraint agents_balance_msats_check check (balance_msats >= -1000000),
  warning_level text not null default 'ok',
  created_at timestamp with time zone not null default now(),
  constraint agents_jwt_sub_unique unique (jwt_sub),
  constraint agents_bolt12_offer_unique unique (bolt12_offer)
);

create index agents_jwt_sub_idx on public.agents using btree (jwt_sub);

create table public.api_keys (
  id uuid not null primary key default gen_random_uuid(),
  user_id uuid not null references public.users (id) on delete cascade,
  key_hash text not null,
  key_prefix text not null,
  name text not null,
  is_active boolean not null default true,
  created_at timestamp with time zone not null default now(),
  last_used_at timestamp with time zone,
  constraint api_keys_key_hash_unique unique (key_hash)
);

create index api_keys_key_hash_idx on public.api_keys using btree (key_hash);
create index api_keys_user_id_idx on public.api_keys using btree (user_id);

-- === supabase/migrations/20260503120003_tier_pricing_templates.sql ===
-- Reference pricing (seed placeholders — replace with real catalog before production)
-- and RunPod template catalog

create table public.tier_pricing (
  tier text not null primary key,
  msats_per_second bigint not null,
  usd_cents_per_second_x100 bigint not null,
  updated_at timestamp with time zone not null default now()
);

comment on table public.tier_pricing is 'Placeholder rates from MVP migration; tune msats_per_second and usd_cents_per_second_x100 for production.';

insert into public.tier_pricing (tier, msats_per_second, usd_cents_per_second_x100) values
  ('nano', 1, 1),
  ('micro', 5, 5),
  ('standard', 20, 20),
  ('gpu_small', 100, 100),
  ('gpu_large', 400, 400);

create table public.templates (
  id uuid not null primary key default gen_random_uuid(),
  name text not null,
  runpod_template_id text not null,
  runtime text not null
    constraint templates_runtime_check check (runtime in ('runpod_cpu', 'runpod_gpu')),
  default_tier text not null references public.tier_pricing (tier) on update cascade on delete restrict,
  created_at timestamp with time zone not null default now(),
  constraint templates_name_unique unique (name)
);

-- === supabase/migrations/20260503120004_sandboxes.sql ===
-- RunPod-backed sandboxes; Lightning (agent_id) or credits (user_id + api_key_id)

create table public.sandboxes (
  id uuid not null primary key default gen_random_uuid(),
  agent_id uuid references public.agents (id) on delete restrict,
  user_id uuid references public.users (id) on delete cascade,
  api_key_id uuid references public.api_keys (id) on delete cascade,
  billing_method text not null
    constraint sandboxes_billing_method_check check (billing_method in ('lightning', 'credits')),
  tier text not null,
  template text not null,
  status text not null,
  runtime text not null
    constraint sandboxes_runtime_check check (runtime in ('runpod_cpu', 'runpod_gpu')),
  pod_id text,
  pod_proxy_url text,
  network_volume_id text,
  created_at timestamp with time zone not null default now(),
  terminated_at timestamp with time zone,
  constraint sandboxes_billing_consistency check (
    (
      billing_method = 'lightning'
      and agent_id is not null
      and user_id is null
      and api_key_id is null
    )
    or (
      billing_method = 'credits'
      and agent_id is null
      and user_id is not null
      and api_key_id is not null
    )
  )
);

create index sandboxes_user_id_idx on public.sandboxes using btree (user_id);
create index sandboxes_agent_id_idx on public.sandboxes using btree (agent_id);
create index sandboxes_api_key_id_idx on public.sandboxes using btree (api_key_id);

-- === supabase/migrations/20260503120005_billing_credit_topups.sql ===
-- Lightning metering, credit ledger, BOLT12 top-ups, manual BOLT11 deposits

create table public.billing_intervals (
  id uuid not null primary key default gen_random_uuid(),
  sandbox_id uuid not null references public.sandboxes (id) on delete cascade,
  agent_id uuid not null references public.agents (id) on delete restrict,
  interval_start timestamp with time zone not null,
  interval_end timestamp with time zone not null,
  msats_consumed bigint not null,
  created_at timestamp with time zone not null default now()
);

create index billing_intervals_sandbox_created_idx
  on public.billing_intervals using btree (sandbox_id, created_at desc);

create index billing_intervals_agent_idx on public.billing_intervals using btree (agent_id);

create table public.credit_transactions (
  id uuid not null primary key default gen_random_uuid(),
  user_id uuid not null references public.users (id) on delete cascade,
  sandbox_id uuid references public.sandboxes (id) on delete set null,
  type text not null,
  amount_usd_cents bigint not null
    constraint credit_transactions_amount_positive check (amount_usd_cents > 0),
  balance_after_usd_cents bigint not null,
  description text not null,
  stripe_payment_intent_id text,
  created_at timestamp with time zone not null default now()
);

create index credit_tx_user_created_idx
  on public.credit_transactions using btree (user_id, created_at desc);

create index credit_tx_type_idx on public.credit_transactions using btree (type);

create table public.top_up_intervals (
  id uuid not null primary key default gen_random_uuid(),
  agent_id uuid not null references public.agents (id) on delete restrict,
  idempotency_key text not null,
  amount_msats bigint not null,
  payment_hash bytea not null,
  preimage bytea,
  bolt12_invoice text,
  status text not null default 'pending',
  requested_at timestamp with time zone not null default now(),
  settled_at timestamp with time zone,
  constraint top_up_intervals_idempotency_key_unique unique (idempotency_key)
);

create index top_up_agent_requested_idx
  on public.top_up_intervals using btree (agent_id, requested_at desc);

create index top_up_payment_hash_idx on public.top_up_intervals using btree (payment_hash);

create table public.pending_deposits (
  id uuid not null primary key default gen_random_uuid(),
  agent_id uuid not null references public.agents (id) on delete restrict,
  payment_hash bytea not null,
  bolt11_invoice text not null,
  amount_msats bigint not null,
  status text not null default 'pending',
  created_at timestamp with time zone not null default now(),
  settled_at timestamp with time zone,
  constraint pending_deposits_payment_hash_unique unique (payment_hash)
);

create index pending_deposits_agent_idx on public.pending_deposits using btree (agent_id);

-- === supabase/migrations/20260503120006_audit_log.sql ===
-- Append-only audit trail (dashboard reads primarily via control plane)

create table public.audit_log (
  id uuid not null primary key default gen_random_uuid(),
  event_type text not null,
  user_id uuid references public.users (id) on delete set null,
  agent_id uuid references public.agents (id) on delete set null,
  sandbox_id uuid references public.sandboxes (id) on delete set null,
  api_key_id uuid references public.api_keys (id) on delete set null,
  auth_method text,
  payload jsonb,
  created_at timestamp with time zone not null default now()
);

create index audit_log_created_idx on public.audit_log using btree (created_at desc);

-- === supabase/migrations/20260503120007_rls.sql ===
-- RLS on all public MVP tables; dashboard SELECT-only where specified in DB.md

alter table public.users enable row level security;

create policy users_select_own
  on public.users
  for select
  to authenticated
  using (id = auth.uid());

alter table public.agents enable row level security;

alter table public.api_keys enable row level security;

create policy api_keys_select_own
  on public.api_keys
  for select
  to authenticated
  using (user_id = auth.uid());

alter table public.sandboxes enable row level security;

create policy sandboxes_select_own
  on public.sandboxes
  for select
  to authenticated
  using (user_id = auth.uid());

alter table public.tier_pricing enable row level security;

create policy tier_pricing_select_all
  on public.tier_pricing
  for select
  to anon, authenticated
  using (true);

alter table public.templates enable row level security;

create policy templates_select_all
  on public.templates
  for select
  to anon, authenticated
  using (true);

alter table public.billing_intervals enable row level security;

alter table public.credit_transactions enable row level security;

create policy credit_transactions_select_own
  on public.credit_transactions
  for select
  to authenticated
  using (user_id = auth.uid());

alter table public.top_up_intervals enable row level security;

alter table public.pending_deposits enable row level security;

alter table public.audit_log enable row level security;

-- === supabase/migrations/20260503120008_auth_user_sync.sql ===
-- Sync public.users on Auth signup (failure blocks signup — test in staging)

create or replace function public.handle_new_user()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
begin
  insert into public.users (id, email)
  values (new.id, new.email);
  return new;
end;
$$;

create trigger on_auth_user_created
  after insert on auth.users
  for each row
  execute procedure public.handle_new_user();

-- === supabase/migrations/20260505120000_agents_anonymous.sql ===
-- Allow Lightning agents created via MCP anonymous L402 (no Supabase user linkage yet).

alter table public.agents alter column jwt_sub drop not null;

