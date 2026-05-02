# Slim Dockerfile — consumes a prebuilt operator binary produced by
# `moon run operator:compile`. All cross-platform / multi-stage logic
# lives in moon tasks; this image is a pure COPY of the static binary.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY bin/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
