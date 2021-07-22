# build container
FROM golang:1.16 as builder
WORKDIR /go/src/github.com/Nexenta/nexentastor-csi-driver-block/
COPY . ./
ARG VERSION
ENV VERSION=$VERSION
RUN make build &&\
    cp ./bin/nexentastor-csi-driver-block /


# driver container
FROM alpine:3.10
LABEL name="nexentastor-block-csi-driver"
LABEL maintainer="Nexenta Systems, Inc."
LABEL description="NexentaStor Block CSI Driver"
LABEL io.k8s.description="NexentaStor Block CSI Driver"
RUN apk update || true &&  \
    apk add coreutils util-linux blkid \
    e2fsprogs bash kmod curl jq ca-certificates

RUN mkdir /nexentastor-csi-driver-block
RUN mkdir -p /etc/ && mkdir -p /config/
COPY --from=builder /nexentastor-csi-driver-block /nexentastor-csi-driver-block/
RUN /nexentastor-csi-driver-block/nexentastor-csi-driver-block --version

ADD chroot-host-wrapper.sh /nexentastor-csi-driver-block

RUN chmod 777 /nexentastor-csi-driver-block/chroot-host-wrapper.sh
RUN    ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/resize2fs \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/findmnt \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/blockdev \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/xfs_growfs \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/blkid \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/e2fsck \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/iscsiadm \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/lsscsi \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/mkfs.ext3 \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/mkfs.ext4 \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/mkfs.xfs \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/multipath \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/multipathd \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/ln \
    && ln -s /nexentastor-csi-driver-block/chroot-host-wrapper.sh /nexentastor-csi-driver-block/mount

ENV PATH="/nexentastor-csi-driver-block/:${PATH}"
ENTRYPOINT ["/nexentastor-csi-driver-block/nexentastor-csi-driver-block"]