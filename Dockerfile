ARG BASE_IMAGE
ARG BUILD_IMAGE

FROM $BASE_IMAGE as builder-iscsi
ARG OPEN_ISCSI_VERSION
ARG OPEN_ISCSI_NAMESPACE
RUN apk add alpine-sdk sudo git
RUN abuild-keygen -n -a -i -q && \
    git clone git://git.alpinelinux.org/aports && \
    cd aports/main/open-iscsi && \
    sed -i -e "s:^pkgrel=[0-9]*$:pkgrel=$OPEN_ISCSI_VERSION:" \
           -e "/build/a sed -i s:ISCSIADM_ABSTRACT_NAMESPACE:$OPEN_ISCSI_NAMESPACE: usr/mgmt_ipc.h" APKBUILD && \
    abuild -F -r -q

FROM $BUILD_IMAGE as builder-driver
ARG TOP_DIR
ARG GIT_CONFIG
ARG GIT_TOKEN
ARG DRIVER_NAME
ARG DRIVER_MODULE
ARG DRIVER_VERSION
ARG DRIVER_PATH
ENV GOPATH $TOP_DIR
WORKDIR $GOPATH/src/$DRIVER_MODULE
COPY . .
RUN apk add --no-cache git make
RUN echo "$GIT_CONFIG" | base64 -d >$HOME/.gitconfig && \
    echo "$GIT_TOKEN" | base64 -d >$HOME/.git-credentials && \
    make DRIVER_PATH=$DRIVER_PATH DRIVER_VERSION=$DRIVER_VERSION build

FROM $BASE_IMAGE
ARG BIN_DIR
ARG ETC_DIR
ARG DRIVER_PATH
ARG OPEN_ISCSI_IQN
LABEL name=$DRIVER_NAME
LABEL maintainer="akhodos@tintri.com"
LABEL description="NexentaStor CSI Block Driver"
LABEL io.k8s.description="NexentaStor CSI Block Driver"
COPY --from=builder-iscsi /root/.abuild/*.pub /etc/apk/keys
COPY --from=builder-iscsi /root/packages /root/packages
RUN echo /root/packages/main >>/etc/apk/repositories
RUN apk add --no-cache open-iscsi e2fsprogs xfsprogs blkid
RUN mkdir -p $BIN_DIR $ETC_DIR
COPY --from=builder-driver $DRIVER_PATH $DRIVER_PATH
RUN $DRIVER_PATH --version
RUN echo '#!/bin/sh -exu' >/init.sh
RUN echo 'mkdir -p /run/lock/iscsi' >>/init.sh
RUN echo "/sbin/iscsi-iname -p $OPEN_ISCSI_IQN | sed s:^:InitiatorName=: >/etc/iscsi/initiatorname.iscsi" >>/init.sh
RUN echo '/bin/cat /etc/iscsi/initiatorname.iscsi' >>/init.sh
RUN echo '/sbin/iscsid' >>/init.sh
RUN echo "$DRIVER_PATH" '"$@"' >>/init.sh
RUN chmod +x /init.sh && cat /init.sh
ENTRYPOINT ["/init.sh"]
