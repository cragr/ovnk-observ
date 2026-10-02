FROM registry.access.redhat.com/ubi9/ubi-micro:latest
COPY bin/ovnk-observ-exporter /usr/bin/
# Node mode connects to root-owned OVN unix sockets.
USER 0
ENTRYPOINT ["/usr/bin/ovnk-observ-exporter"]
