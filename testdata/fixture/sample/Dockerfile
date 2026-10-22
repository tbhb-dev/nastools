# The image that both services of the sample project run. A container
# reports healthy READY_DELAY seconds after it starts and stays in the
# starting state until then, which holds a deploy in `compose up --wait`.
FROM busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e

HEALTHCHECK --interval=1s --timeout=1s --start-period=5m --retries=3 CMD ["test", "-f", "/tmp/ready"]
CMD ["sh", "-c", "sleep \"${READY_DELAY:-0}\" && touch /tmp/ready && echo ready && exec sleep 2147483647"]
