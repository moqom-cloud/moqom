# Field test: direct paths across real networks

How often a 1:1 call can go direct depends on the NATs in front of the two devices, and nothing
in CI can say. This test measures it before any site quotes a rate.

## What decides it

A direct path opens when each side's NAT keeps one public port for one socket, whoever it sends
to (endpoint-independent mapping). Then the address the relay's [STUN](https://www.rfc-editor.org/rfc/rfc8489) responder reports is one the
peer can reach. A symmetric NAT gives a new port per destination, so the call stays on the relay.
IPv6 usually has no NAT at all, only a stateful firewall, which simultaneous probes get through.

## Step 1: classify each network

`moqom-nat-probe` (in `moqom-engine/moqom-loadgen`) asks several [STUN](https://www.rfc-editor.org/rfc/rfc8489) servers from one socket per
address family and says which case the network is. It needs servers on at least two addresses.

```sh
cargo run -p moqom-loadgen --bin moqom-nat-probe -- \
  --stun <us-west1 relay>:3478 --stun <us-west2 relay>:3478 \
  --stun '[<us-west1 relay IPv6>]:3478' --stun '[<us-west2 relay IPv6>]:3478' \
  --label "home wifi, <ISP>"
```

The addresses are `terraform output relay_endpoints` and `relay_endpoints_v6`. Two regions,
not the edge name, because the edge name steers both lookups to the same relay.

Until the relays answer STUN in production, two public servers stand in:
`--stun stun.l.google.com:19302 --stun stun.cloudflare.com:3478`.

Cover at least: three home ISPs (cable, fibre, DSL), an office network, a coffee shop or hotel,
and each US carrier on a phone. A laptop on a phone's hotspot adds the phone's own NAT, so for
cellular also run step 2 on the phone itself.

## Step 2: real calls

After the relays are deployed with [STUN](https://www.rfc-editor.org/rfc/rfc8489) and IPv6 (`deploy/`, not yet applied), place a 1:1 call
between two iPhones for each pair of network types above, and record whether it went direct, on
which family, and how long it took. Each pair also covers the reverse direction.

## Pass criteria

- Every call connects, whatever its path. Relay fallback is not optional.
- The site's wording follows the results: "most" only if more than half the pairs go direct.

## Results

| Network | IPv4 | IPv6 |
| --- | --- | --- |
| Dev workstation, Spectrum (8 Oct 2026) | symmetric NAT: direct paths stay on the relay | endpoint-independent mapping: hole-punching can work |

| Call pair | Path | Family | Time to direct |
| --- | --- | --- | --- |
| | | | |
