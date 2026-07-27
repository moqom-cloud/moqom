# Licensing

**Current status: all rights reserved.** No licence is granted. This is a
holding position, not the intended end state.

## Why nothing is set yet

The plan calls for two different licences in one repository:

- **Apache-2.0** for the SDKs, protocol implementation, schemas, and samples —
  the parts people integrate against, where permissive licensing is the whole
  point.
- **Source-available** (BSL-style, with a conversion date and a competing-service
  restriction) for the relay and control plane — source-available rather than
  closed, because asking security-conscious customers to run an opaque binary
  while claiming auditability is a contradiction.

Applying Apache-2.0 at the repository root now and carving out the relay later
does not work. A permissive grant cannot be withdrawn from code already
published under it, so the root licence has to be the *narrower* of the two, or
absent, until the boundary is drawn.

## Before the first outside contribution

Three things must be settled, and the order matters:

1. **The Apache/source-available boundary**, expressed as directory-scoped
   `LICENSE` files and a `license` field on every crate.
2. **A CLA**, not a DCO. Both relicensing and acquisition require the right to
   change terms, and retrofitting a CLA means tracking down every past
   contributor — the cost grows with every merged PR.
3. **The patent position.** Apache-2.0 carries an express patent grant, so it
   cannot be decided independently of any patent strategy. Note also that IETF
   BCP 79 imposes disclosure obligations if we contribute to MoQ drafts while
   pursuing patents that read on them.

On `moqom-cc` specifically: a patent publishes the design, and controller
tuning is difficult to reverse-engineer from observed traffic. Trade secret may
protect it better than a patent would.

Tracked as the `licensing` todo.
