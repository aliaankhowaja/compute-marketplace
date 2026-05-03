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
