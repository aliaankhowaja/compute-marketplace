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
