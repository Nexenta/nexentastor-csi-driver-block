ARG BASE_IMAGE
ARG BUILD_IMAGE

# iscsi build container
FROM $BASE_IMAGE as builder-iscsi
ARG OPEN_ISCSI_VERSION
ARG OPEN_ISCSI_NAMESPACE
ARG ALPINE_VERSION

RUN apk add --update python3-dev py3-pip build-base cairo cairo-dev cargo freetype-dev \
  gcc gdk-pixbuf-dev gettext jpeg-dev lcms2-dev libffi-dev musl-dev openjpeg-dev openssl-dev \
  pango-dev poppler-utils postgresql-client postgresql-dev py-cffi python3-dev rust tcl-dev \
  tiff-dev tk-dev zlib-dev alpine-sdk sudo git openrc open-iscsi autoconf py3-cryptography
RUN abuild-keygen -n -a -i -q && \
    git clone -b $ALPINE_VERSION-stable https://git.alpinelinux.org/aports
WORKDIR /aports/main/open-iscsi
RUN sed -i -e "s:^pkgrel=[0-9]*$:pkgrel=$OPEN_ISCSI_VERSION:" \
    -e "/build()/a sed -i s:ISCSIADM_ABSTRACT_NAMESPACE:$OPEN_ISCSI_NAMESPACE: usr/mgmt_ipc.h" \
    -e "s/openssl-dev>3/openssl-dev/" APKBUILD
RUN abuild -F -r -q

# build container
FROM $BUILD_IMAGE as builder
WORKDIR /go/src/github.com/Nexenta/nexentastor-csi-driver-block/
COPY . ./
ARG VERSION
ENV VERSION=$VERSION
RUN apk add --no-cache make git
RUN make build &&\
    cp ./bin/nexentastor-csi-driver-block /

# driver container
FROM $BASE_IMAGE
LABEL name="nexentastor-block-csi-driver"
LABEL maintainer="akhodos@tintri.com"
LABEL description="NexentaStor Block CSI Driver"
LABEL io.k8s.description="NexentaStor Block CSI Driver"
COPY --from=builder-iscsi /root/.abuild/*.pub /etc/apk/keys
COPY --from=builder-iscsi /root/packages /root/packages
RUN echo /root/packages/main >>/etc/apk/repositories
RUN mkdir -p /etc/ && mkdir -p /config/
RUN apk update
RUN apk add --no-cache kmod coreutils util-linux open-iscsi e2fsprogs xfsprogs blkid findmnt ca-certificates
RUN apk add "libcrypto3>=3.0.8-r3" "libssl3>=3.0.8-r3" && rm -rf /var/cache/apt/*
COPY --from=builder /nexentastor-csi-driver-block /nexentastor-csi-driver-block/
RUN echo '#!/bin/sh -exu' >/init.sh
RUN echo 'mkdir -p /run/lock/iscsi' >>/init.sh
RUN cp -r /usr/sbin/* /sbin/
# RUN echo "/sbin/iscsi-iname -p $OPEN_ISCSI_IQN | sed s:^:InitiatorName=: >/etc/iscsi/initiatorname.iscsi" >>/init.sh
# RUN echo '/bin/cat /etc/iscsi/initiatorname.iscsi' >>/init.sh
RUN mkdir -p /etc/iscsi
COPY initiatorname.iscsi /etc/iscsi/initiatorname.iscsi
RUN echo '/sbin/iscsid' >>/init.sh
RUN echo 'update-ca-certificates\n/nexentastor-csi-driver-block/nexentastor-csi-driver-block "$@"' >>/init.sh
RUN chmod +x /init.sh && cat /init.sh
ENTRYPOINT ["/init.sh"]



# # driver container
# FROM $BASE_IMAGE
# ARG BIN_DIR
# ARG ETC_DIR
# ARG DRIVER_PATH
# ARG OPEN_ISCSI_IQN
# LABEL name="nexentastor-block-csi-driver"
# LABEL maintainer="akhodos@tintri.com"
# LABEL description="NexentaStor Block CSI Driver"
# LABEL io.k8s.description="NexentaStor Block CSI Driver"
# COPY --from=builder-iscsi /root/.abuild/*.pub /etc/apk/keys
# COPY --from=builder-iscsi /root/packages /root/packages
# # RUN apk update || true &&  \
# #     apk add coreutils util-linux blkid open-iscsi xfsprogs findmnt \
# #     e2fsprogs kmod curl jq ca-certificates
# RUN apk add --no-cache open-iscsi e2fsprogs xfsprogs blkid findmnt ca-certificates

# RUN apk update && apk add "libcrypto3>=3.0.8-r3" "libssl3>=3.0.8-r3" && rm -rf /var/cache/apt/*
# RUN mkdir /nexentastor-csi-driver-block
# RUN cp -r /usr/sbin/* /sbin/
# RUN mkdir -p /etc/ && mkdir -p /config/

# RUN echo '#!/bin/sh -exu' >/init.sh
# RUN echo 'mkdir -p /run/lock/iscsi' >>/init.sh
# RUN cp -r /usr/sbin/* /sbin/
# RUN echo "/sbin/iscsi-iname -p $OPEN_ISCSI_IQN | sed s:^:InitiatorName=: >/etc/iscsi/initiatorname.iscsi" >>/init.sh
# RUN echo '/bin/cat /etc/iscsi/initiatorname.iscsi' >>/init.sh
# RUN echo '/sbin/iscsid' >>/init.sh


# # RUN echo '#!/bin/sh -exu' >/init.sh
# # RUN echo 'mkdir -p /run/lock/iscsi' >>/init.sh
# # RUN echo "/sbin/iscsi-iname -p $OPEN_ISCSI_IQN | sed s:^:InitiatorName=: >/etc/iscsi/initiatorname.iscsi" >>/init.sh
# # RUN echo '/bin/cat /etc/iscsi/initiatorname.iscsi' >>/init.sh
# # RUN echo '/sbin/iscsid || true' >>/init.sh

# COPY --from=builder /nexentastor-csi-driver-block /nexentastor-csi-driver-block/
# RUN /nexentastor-csi-driver-block/nexentastor-csi-driver-block --version

# RUN echo 'update-ca-certificates\n/nexentastor-csi-driver-block/nexentastor-csi-driver-block "$@";\n' >> /init.sh
# ENV PATH="/nexentastor-csi-driver-block/:${PATH}"
# RUN chmod +x /init.sh && cat /init.sh
# ENTRYPOINT ["/init.sh"]
