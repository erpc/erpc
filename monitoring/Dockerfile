FROM prom/prometheus:v3.12.0@sha256:69f5241418838263316593f7274a304b095c40bcf22e57272865da91bd60a8ac as prometheus
FROM grafana/grafana:12.4.5@sha256:26b8f35a9e4e4431995cf64c3f396505a4faf17bcfc19f9ed84943ec6bfd5ecd as grafana

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
