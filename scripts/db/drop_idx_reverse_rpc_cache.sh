#!/usr/bin/env bash
set -euo pipefail

NS="${NS:-morpho-prd}"
POD="${POD:-erpc-db-1}"
DB="${DB:-erpc}"
TABLE="${TABLE:-rpc_cache}"

# Drop redundant idx_reverse (if it duplicates PK) while ensuring the real reverse-lookup index exists.
# Runs entirely via kubectl exec (no local port-forward / creds needed).

psql_in_pod() {
  kubectl exec -n "$NS" "$POD" -c postgres -- bash -lc "psql -U postgres -d '$DB' -v ON_ERROR_STOP=1 -c \"$1\""
}

note() { echo "[db-migrate] $*"; }

action="${1:-apply}"
if [[ "$action" != "apply" && "$action" != "dry-run" ]]; then
  echo "usage: $0 [apply|dry-run]" >&2
  exit 2
fi

note "context=$(kubectl config current-context) ns=$NS pod=$POD db=$DB table=$TABLE mode=$action"

note "checking indexes"
psql_in_pod "select indexname, indexdef from pg_indexes where schemaname='public' and tablename='$TABLE' order by indexname;"

note "ensuring idx_range_partition exists (needed for wildcard reverse lookups)"
if [[ "$action" == "apply" ]]; then
  # CONCURRENTLY: no long table write lock; must be top-level statement.
  psql_in_pod "create index concurrently if not exists idx_range_partition on $TABLE using btree (range_key, partition_key);"
else
  note "dry-run: would run: create index concurrently if not exists idx_range_partition ..."
fi

note "checking whether idx_reverse duplicates PK"
# We only drop idx_reverse if it is exactly (partition_key, range_key) like the PK.
# We do NOT drop it if it is the (range_key, partition_key) reverse index.
psql_in_pod "\
with idx as (\
  select i.indexrelid::regclass::text as idxname,\
         pg_get_indexdef(i.indexrelid) as def\
  from pg_index i\
  join pg_class c on c.oid=i.indrelid\
  where c.relname='$TABLE'\
    and i.indexrelid::regclass::text in ('idx_reverse','${TABLE}_pkey','rpc_cache_pkey')\
)\
select * from idx order by idxname;\
"

# Extract definition; avoid parsing in bash with regex heavy logic; use SQL predicate.
redundant=$(kubectl exec -n "$NS" "$POD" -c postgres -- bash -lc "psql -U postgres -d '$DB' -At -v ON_ERROR_STOP=1 -c \"\
select exists (\
  select 1\
  from pg_index i\
  join pg_class t on t.oid=i.indrelid\
  join pg_class ic on ic.oid=i.indexrelid\
  where t.relname='$TABLE'\
    and ic.relname='idx_reverse'\
    and pg_get_indexdef(i.indexrelid) like 'CREATE INDEX idx_reverse ON public.$TABLE USING btree (partition_key, range_key)%'\
);\
\"" | tr -d '\r')

if [[ "$redundant" != "t" && "$redundant" != "f" ]]; then
  echo "unexpected predicate result: $redundant" >&2
  exit 1
fi

if [[ "$redundant" == "f" ]]; then
  note "idx_reverse not recognized as PK-duplicate; skipping drop"
  exit 0
fi

note "idx_reverse looks redundant (PK-duplicate). dropping concurrently"
if [[ "$action" == "apply" ]]; then
  psql_in_pod "drop index concurrently if exists idx_reverse;"
else
  note "dry-run: would run: drop index concurrently if exists idx_reverse;"
fi

note "post-drop indexes"
psql_in_pod "select indexname, indexdef from pg_indexes where schemaname='public' and tablename='$TABLE' order by indexname;"

note "index sizes"
psql_in_pod "select relname, pg_size_pretty(pg_relation_size(oid)) sz from pg_class where relname in ('rpc_cache_pkey','idx_reverse','idx_range_partition') order by relname;"

note "done"
