# Network security

State as verified read-only on 2026-10-06, and the intended state the nightly
check enforces (rules NET-001..004, GKE-001..005, SQL-004/005).

## Intended exposure

Only these are reachable from the internet:

| Port | Service | Why public |
| --- | --- | --- |
| UDP/443 | Relays ([QUIC](https://www.rfc-editor.org/rfc/rfc9000), MOQT), us-west1 and us-west2 | The product |
| TCP/443 | HTTPS load balancer for the sites (and the API when it is fronted) | Websites |
| TCP/8444 | Control-plane join endpoint (35.197.36.243) | Devices obtain credentials before they have any; requires no client certificate by design, bounded by device accounts |

Everything else is private: GKE nodes have no external IPs; Cloud SQL has
private IP only and `ENCRYPTED_ONLY` SSL; the control plane's gRPC/admin port
needs mutual TLS; outbound goes through Cloud NAT.

The compliance check's allowlist is `-public-ingress` (default
`udp:443,tcp:443,tcp:8444`). A world-open rule outside it is HIGH, or CRITICAL
on SSH, RDP, Postgres, MySQL, the Kubernetes API, kubelet or etcd ports.

## Verified state and gaps

| Item | Found | Severity | Fix |
| --- | --- | --- | --- |
| `default` VPC with `default-allow-ssh` (tcp:22), `default-allow-rdp` (tcp:3389), `default-allow-icmp` from 0.0.0.0/0 | Present | **CRITICAL** | Nothing MOQOM runs is on the default network. Delete the three rules, then the network (`gcloud compute firewall-rules delete default-allow-ssh default-allow-rdp default-allow-icmp default-allow-internal`; `gcloud compute networks delete default`). Confirm first that no instance uses it |
| GKE-managed `k8s-fw-*` rules: 0.0.0.0/0 on tcp 8080/8443/8444 to private nodes | Present | LOW (managed) | Created by LoadBalancer Services. Set `loadBalancerSourceRanges` where a port need not be public (8080 health, 8443) |
| GKE control-plane endpoint public, no authorized networks | Present | MEDIUM | Enable master authorized networks (owner's IP or Cloud Shell), or the DNS-based endpoint with IAM |
| Firewall rule logging off on world-open rules | Present | LOW | Enable on the remaining public rules |
| GKE Secrets not encrypted with a customer key | Present | INFO | Optional: application-layer secrets encryption with Cloud KMS |
| SSL policy `moqom-sites`: MODERN, min TLS 1.2 | Pass | — | — |
| Private nodes, Workload Identity, release channel REGULAR | Pass | — | — |
| Cloud SQL: no public IP, ENCRYPTED_ONLY | Pass | — | — |

## Transport encryption

- [QUIC](https://www.rfc-editor.org/rfc/rfc9000) (relays, clients): [TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446) by protocol.
- RelayLink (relay ↔ control plane gRPC): mutual TLS with a deployment CA,
  plus a per-relay bearer credential. No plaintext fallback.
- Join endpoint: server TLS (same certificate), no client certificate.
- HTTPS LB: MODERN profile, TLS ≥1.2.
- Cloud SQL: Cloud SQL connector with IAM authentication; `ENCRYPTED_ONLY`.
- Media: [SFrame](https://www.rfc-editor.org/rfc/rfc9605) end-to-end where the client enables it (Swift SDK). Relays
  forward ciphertext and hold no media keys.

### Post-quantum key exchange

- Every TLS configuration in the Rust engine puts the hybrid group
  [X25519MLKEM768](https://datatracker.ietf.org/doc/draft-ietf-tls-ecdhe-mlkem/) ([ML-KEM-768](https://csrc.nist.gov/pubs/fips/203/final) combined with X25519) first, with plain X25519
  second. That covers the relay listener (raw [QUIC](https://www.rfc-editor.org/rfc/rfc9000) and [WebTransport](https://datatracker.ietf.org/doc/draft-ietf-webtrans-http3/)), the
  client SDK, relay-to-relay links, direct peer-to-peer paths, and the
  relay's and recorder's gRPC. For the relay listener and both ends of a
  direct path, tests complete real handshakes and check that the hybrid
  group was negotiated.
- The Go control plane uses Go's default curve list, which includes
  X25519MLKEM768. No `CurvePreferences` override exists.
- A peer that offers only classical groups still connects. An X25519-only
  peer gets X25519 with no extra round trip. A peer with neither group costs
  one HelloRetryRequest.
- What this protects is the key exchange: a recording of the traffic can't
  be decrypted later by breaking X25519 alone. Certificates and signatures
  are still classical (ECDSA or RSA), so authentication isn't post-quantum.
- The Apple clients' key exchange is set by the platform TLS stack. Checked
  once by hand on macOS 26.4.1 (7 October 2026): a Network.framework QUIC
  client, configured as MoqomKit's, offered `X25519MLKEM768, X25519,
  secp256r1, secp384r1, secp521r1` (after a GREASE value) to a rustls server
  with the engine's settings, so the hybrid group is chosen. Not yet checked
  on an iPhone, and not covered by automated tests.
- End-to-end media keys come from [MLS](https://www.rfc-editor.org/rfc/rfc9420), not TLS. Every room's MLS group uses
  `MLS_256_MLKEM1024_AES256GCM_SHA512_MLDSA87` (`0xF0AC`): pure ML-KEM-1024 key
  encapsulation and ML-DSA-87 signatures; see
  `docs/E2EE-KEY-MANAGEMENT.md` for what that does and doesn't
  cover.

## Edge protection

No WAF or DDoS service sits in front of the relays or join endpoint today
(NET-6, Planned). Google's network absorbs volumetric attacks at the edge for
load-balanced traffic; the relays are on passthrough addresses. Cloudflare in
front of the sites and DNS is Planned (it would become a subprocessor).

## Change detection

Firewall changes alert (`firewall-changes` metric), and the nightly check
compares every rule against the allowlist.
