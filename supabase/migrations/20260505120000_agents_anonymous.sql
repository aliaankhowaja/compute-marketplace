-- Allow Lightning agents created via MCP anonymous L402 (no Supabase user linkage yet).
alter table public.agents alter column jwt_sub drop not null;
