FROM ubuntu:20.04

RUN apt-get update \
 && apt-get install -y openssl libapr1 libnuma1 \
    libmbedcrypto3 libmbedtls12 libmbedx509-0 libsubunit0 \
    iproute2  libnl-3-dev libnl-route-3-dev python3 iputils-ping tcpdump \
 && rm -rf /var/lib/apt/lists/*

COPY debs/*.deb /tmp/

RUN dpkg -i \
  /tmp/libvppinfra*.deb \
  /tmp/libvppinfra-dev*.deb \
  /tmp/vpp*.deb \
  /tmp/vpp-plugin-core*.deb \
  /tmp/python3-vpp-api*.deb

ADD startup_srv.conf /
ADD srv.conf /

CMD /usr/bin/vpp -c /startup_srv.conf
