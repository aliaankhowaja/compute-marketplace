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
