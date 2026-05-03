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
