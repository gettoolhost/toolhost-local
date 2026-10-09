# Release image — GoReleaser (dockers_v2) builds the binaries for every
# platform and drops them into the build context as $TARGETPLATFORM/toolhost.
# distroless/static: the smallest honest container that still has CA roots —
# a scratch image can't TLS-verify HTTPS upstream backends — plus a /tmp and
# a nonroot user.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/toolhost /usr/bin/toolhost
ENTRYPOINT ["/usr/bin/toolhost"]
CMD ["serve", "--stdio"]
