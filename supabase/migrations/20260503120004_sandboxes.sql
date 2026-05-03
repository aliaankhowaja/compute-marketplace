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
