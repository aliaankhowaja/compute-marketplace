-- RunPod templates are resolved from env (RUNPOD_TEMPLATE_*) keyed by sandbox tier.

drop policy if exists templates_select_all on public.templates;
drop table if exists public.templates;
