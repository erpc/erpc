FROM prom/prometheus:v3.8.1@sha256:2b6f734e372c1b4717008f7d0a0152316aedd4d13ae17ef1e3268dbfaf68041b as prometheus
FROM grafana/grafana:12.3.1@sha256:2175aaa91c96733d86d31cf270d5310b278654b03f5718c59de12a865380a31f as grafana

FROM ubuntu:22.10@sha256:e322f4808315c387868a9135beeb11435b5b83130a8599fd7d0014452c34f489

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