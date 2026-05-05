-- Per-sandbox prepaid balance + opaque bearer token (replaces agent-scoped L402 flow).

alter table public.sandboxes
  add column sandbox_token text,
  add column prepaid_balance_msats bigint not null default 0
    constraint sandboxes_prepaid_balance_msats_check check (prepaid_balance_msats >= -1000000),
  add column pending_invoice_bolt11 text,
  add column pending_invoice_payment_hash bytea,
  add column pending_invoice_amount_msats bigint,
  add column pending_invoice_expires_at timestamptz;

create unique index sandboxes_sandbox_token_uniq
  on public.sandboxes (sandbox_token) where sandbox_token is not null;

create index sandboxes_pending_invoice_payment_hash_idx
  on public.sandboxes (pending_invoice_payment_hash)
  where pending_invoice_payment_hash is not null;

-- Loosen the billing-consistency check: lightning sandboxes can have NULL agent_id
-- when they use a sandbox_token (anonymous lightning); agent-based rows still allowed for legacy data.
alter table public.sandboxes drop constraint sandboxes_billing_consistency;
alter table public.sandboxes add constraint sandboxes_billing_consistency check (
  (
    billing_method = 'lightning'
    and user_id is null
    and api_key_id is null
    and (
      (agent_id is not null and sandbox_token is null)
      or (agent_id is null and sandbox_token is not null)
    )
  )
  or (
    billing_method = 'credits'
    and agent_id is null
    and user_id is not null
    and api_key_id is not null
    and sandbox_token is null
  )
);

-- pending_deposits: allow sandbox-scoped deposits (agent_id becomes nullable)
alter table public.pending_deposits
  alter column agent_id drop not null,
  add column sandbox_id uuid references public.sandboxes(id) on delete cascade;

create index pending_deposits_sandbox_id_idx
  on public.pending_deposits (sandbox_id) where sandbox_id is not null;

alter table public.pending_deposits
  add constraint pending_deposits_owner_check check (
    (agent_id is not null and sandbox_id is null)
    or (agent_id is null and sandbox_id is not null)
  );
