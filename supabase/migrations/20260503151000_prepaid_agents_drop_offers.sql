-- Prepaid Lightning: drop outbound pull-pay offer metadata from agents.

alter table public.agents drop constraint if exists agents_bolt12_offer_unique;
alter table public.agents drop constraint if exists agents_offer_type_check;

alter table public.agents drop column if exists bolt12_offer;
alter table public.agents drop column if exists lightning_address;
alter table public.agents drop column if exists offer_type;
