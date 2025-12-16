FROM prom/prometheus:v3.8.1@sha256:2b6f734e372c1b4717008f7d0a0152316aedd4d13ae17ef1e3268dbfaf68041b as prometheus
FROM grafana/grafana:9.3.2@sha256:2a73ae33c9f0c51af6eced2ef185d5d3682b4c378c4fdd6941a14e8ea4a3e95b as grafana

FROM ubuntu:22.04@sha256:09506232a8004baa32c47d68f1e5c307d648fdd59f5e7eaa42aaf87914100db3

RUN apt-get update && apt-get install -y curl

# Copy Prometheus files
COPY --from=prometheus /bin/prometheus /bin/prometheus
COPY --from=prometheus /bin/promtool /bin/promtool
COPY prometheus/prometheus.yml /etc/prometheus/prometheus.yml
COPY prometheus/alert.rules /etc/prometheus/alert.rules

# Copy Grafana files
COPY --from=grafana /usr/share/grafana /usr/share/grafana
COPY --from=grafana /etc/grafana /etc/grafana
COPY grafana/grafana.ini /etc/grafana/grafana.ini
COPY grafana/datasources/prometheus.yml /etc/grafana/provisioning/datasources/prometheus.yml
COPY grafana/dashboards/default.yml /etc/grafana/provisioning/dashboards/default.yml
COPY grafana/dashboards/erpc.json /etc/grafana/provisioning/dashboards/erpc.json

# Set up entrypoint script
COPY scripts/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 3000 9090

ENTRYPOINT ["/entrypoint.sh"]