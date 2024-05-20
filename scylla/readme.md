# Local Scylla Setup

1. Start Scylla in a docker container:
```bash
cd ./erpc

make up
docker exec -it erpc-scylla cqlsh
```

2. Create the keyspace and tables:
```sql
CREATE KEYSPACE erpc WITH REPLICATION = {'class': 'SimpleStrategy', 'replication_factor': 1};
USE erpc;
CREATE TABLE rpc_cache (
    key varchar,
    value text,
    timestamp timestamp,
    primary key(key, timestamp)
) WITH CLUSTERING ORDER BY (timestamp DESC)
AND gc_grace_seconds = 0
AND COMPACTION = {
    'class': 'TimeWindowCompactionStrategy',
    'compaction_window_unit' : 'HOURS',
    'compaction_window_size' : 1
};
INSERT INTO rpc_cache (key, value, timestamp) VALUES ('key1', 'value1', toTimestamp(now()));
```