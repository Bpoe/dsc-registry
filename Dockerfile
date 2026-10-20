FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
ARG TARGETARCH
# This pinned base includes the CA bundle on both supported architectures.
RUN mkdir -p /etc/dscregd && \
    mkdir -m 0700 /reports && chown 65532:65532 /reports
COPY --chmod=0755 dist/container/${TARGETARCH}/dscregd /usr/local/bin/dscregd
LABEL org.opencontainers.image.source="https://github.com/Bpoe/dsc-registry"
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["dscregd"]
CMD ["-config", "/etc/dscregd/config.json"]
