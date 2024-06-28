#!/bin/bash

# Replace placeholder values in Prometheus config
sed -i "s/REPLACE_SERVICE_ENDPOINT_HERE:REPLACE_SERVICE_PORT_HERE/${SERVICE_ENDPOINT}:${SERVICE_PORT}/g" /etc/prometheus/prometheus.yml

# Start Prometheus
/bin/prometheus --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus &

# Start Grafana
/usr/share/grafana/bin/grafana-server \
  --homepath=/usr/share/grafana \
  --config=/etc/grafana/grafana.ini \
  cfg:default.paths.data=/var/lib/grafana \
  cfg:default.paths.logs=/var/log/grafana \
  cfg:default.paths.plugins=/var/lib/grafana/plugins \
  web.enable-gzip=true \
  web.address=0.0.0.0 &

# Keep the container running
tail -f /dev/null