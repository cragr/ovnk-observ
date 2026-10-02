FROM registry.access.redhat.com/ubi9/ubi-micro:9.6
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/cragr/ovnk-observ" \
      org.opencontainers.image.title="ovnk-observ-exporter" \
      org.opencontainers.image.description="OVN-Kubernetes NB/SB database and network object scale exporter" \
      org.opencontainers.image.version="${VERSION}"
COPY bin/ovnk-observ-exporter /usr/bin/
# Node mode connects to root-owned OVN unix sockets.
USER 0
ENTRYPOINT ["/usr/bin/ovnk-observ-exporter"]
