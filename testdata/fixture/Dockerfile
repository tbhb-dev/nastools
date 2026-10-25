# An SSH engine fixture: a stand-in for the device that nastools deploys to.
# sshd accepts public keys only, and the fixture user may run exactly the
# engine forwarder through sudo. The Docker CLI reaches the engine whose
# socket is mounted at /var/run/docker.sock. The host key and the
# authorized key are copied in before the container starts, so the image
# holds no key material.
FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

RUN apk add --no-cache docker-cli flock jq openssh-server shadow sudo \
  && ln -s /usr/bin/docker /usr/local/bin/docker \
  && useradd --create-home --shell /bin/sh nastools \
  && echo 'nastools:*' | chpasswd -e \
  && install -d -m 700 -o nastools -g nastools /home/nastools/.ssh \
  && install -d -o nastools -g nastools /var/lib/nastools /var/lib/nastools/locks

COPY sshd_config /etc/ssh/sshd_config
COPY --chmod=440 sudoers /etc/sudoers.d/nastools
COPY --chmod=755 tailscale.sh /usr/local/bin/tailscale
COPY --chmod=755 flock.sh /usr/local/bin/flock
COPY --chmod=755 start.sh /usr/local/sbin/start-fixture

EXPOSE 22
CMD ["/usr/local/sbin/start-fixture"]
