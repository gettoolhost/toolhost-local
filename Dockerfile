# Release image — GoReleaser (dockers_v2) builds the binaries for every
# platform and drops them into the build context as $TARGETPLATFORM/toolhost.
# Scratch + static binary: the smallest honest container for a gateway.
FROM scratch
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/toolhost /usr/bin/toolhost
ENTRYPOINT ["/usr/bin/toolhost"]
CMD ["serve", "--stdio"]
