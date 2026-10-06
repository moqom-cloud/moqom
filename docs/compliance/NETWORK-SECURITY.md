# Network security

State as verified read-only on 2026-10-06, and the intended state the nightly
check enforces (rules NET-001..004, GKE-001..005, SQL-004/005).

## Intended exposure

Only these are reachable from the internet:

| Port | Service | Why public |
| --- | --- | --- |
| UDP/443 | Relays (QUIC, MOQT), us-west1 and us-west2 | The product |
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

- QUIC (relays, clients): TLS 1.3 by protocol.
- RelayLink (relay ↔ control plane gRPC): mutual TLS with a deployment CA,
  plus a per-relay bearer credential. No plaintext fallback.
- Join endpoint: server TLS (same certificate), no client certificate.
- HTTPS LB: MODERN profile, TLS ≥1.2.
- Cloud SQL: Cloud SQL connector with IAM authentication; `ENCRYPTED_ONLY`.
- Media: SFrame end-to-end where the client enables it (Swift SDK). Relays
  forward ciphertext and hold no media keys.

## Edge protection

No WAF or DDoS service sits in front of the relays or join endpoint today
(NET-6, Planned). Google's network absorbs volumetric attacks at the edge for
load-balanced traffic; the relays are on passthrough addresses. Cloudflare in
front of the sites and DNS is Planned (it would become a subprocessor).

## Change detection

Firewall changes alert (`firewall-changes` metric), and the nightly check
compares every rule against the allowlist.
