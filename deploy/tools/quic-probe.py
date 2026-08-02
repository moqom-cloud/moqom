#!/usr/bin/env python3
"""Ask a relay whether its QUIC socket is really reachable.

There is no UDP health check in Google Cloud, so nothing in the load balancer's
view of the fleet observes the port that carries all of the media. The TCP probe
on the admin port answers "is this process alive", which is a different question
from "can a datagram from the internet reach its QUIC socket" -- and the two
diverged twice while this tier was being built, once at the VPC firewall and
once at the host's own.

This closes that gap without a full client. It sends a QUIC Initial naming a
version no one implements, which the specification requires a server to answer
with a Version Negotiation packet. Getting one back proves the whole path: the
forwarding rule, the backend selection, the host firewall, and a process bound
to the socket and reading from it. Getting silence proves one of them is broken,
and says nothing about which -- but silence is the answer that matters, because
the TCP probe cannot produce it.

    deploy/tools/quic-probe.py 34.53.113.58 [port]
"""

import os
import socket
import sys


# Any value with the pattern 0x?a?a?a?a is reserved to force version
# negotiation, and is guaranteed never to be a version a server speaks.
FORCE_NEGOTIATION = 0x1A2A3A4A

# The minimum size of a datagram carrying an Initial. A server is required to
# discard anything smaller, which is the anti-amplification rule; a probe that
# forgets the padding looks exactly like an unreachable server.
SMALLEST_INITIAL = 1200


def initial_packet() -> bytes:
    destination = os.urandom(8)
    source = os.urandom(8)

    header = bytearray()
    # Long header, fixed bit, Initial, two-byte packet number.
    header.append(0xC1)
    header += FORCE_NEGOTIATION.to_bytes(4, "big")
    header.append(len(destination))
    header += destination
    header.append(len(source))
    header += source
    header.append(0x00)  # Token length, as a one-byte varint: no token.

    # Length and packet number, then padding out to the minimum. The contents
    # are nonsense and it does not matter: a server checks the version before it
    # tries to decrypt anything.
    body = b"\x00\x00" + bytes(SMALLEST_INITIAL - len(header) - 4)
    length = (0x4000 | len(body)).to_bytes(2, "big")  # Two-byte varint.

    return bytes(header) + length + body


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__.strip().splitlines()[-1].strip(), file=sys.stderr)
        return 2

    host = sys.argv[1]
    port = int(sys.argv[2]) if len(sys.argv) > 2 else 443

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(5)
    sock.sendto(initial_packet(), (host, port))

    try:
        reply, _ = sock.recvfrom(2048)
    except socket.timeout:
        print(f"{host}:{port} did not answer within 5s")
        return 1

    # A Version Negotiation packet is a long header whose version is zero.
    negotiated = len(reply) >= 5 and reply[0] & 0x80 and reply[1:5] == b"\x00\x00\x00\x00"
    if not negotiated:
        print(f"{host}:{port} answered {len(reply)} bytes, but not with version negotiation")
        return 1

    versions = [
        int.from_bytes(reply[i : i + 4], "big")
        for i in range(7 + reply[5] + reply[6 + reply[5]], len(reply), 4)
    ]
    offered = ", ".join(f"0x{v:08x}" for v in versions) or "none"
    print(f"{host}:{port} is serving QUIC; it offers {offered}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
