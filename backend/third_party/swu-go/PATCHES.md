# Local portability and reader-VoWiFi patches

This directory is based on `github.com/1239t/swu-go v0.0.1`, the version used
by `vowifi-go` at commit `21eb46189e0ab82c56c791c4834a9383078f9c5f`.

The local changes are intentionally limited to:

- macOS build constraints/stubs for Linux-only XFRM, network namespace,
  serial, and error-queue code;
- system-resolver/fake-IP operation without a second outbound-IP probe socket;
- a carrier-provided IKE local identifier and the Verizon NAT-T port exception;
- the AES-256/SHA-512 and AES-256/SHA-256 IKE/ESP proposals present in common
  Apple carrier bundles; and
- IPv6-safe remote host/port joining.

Reader mode uses `DataplaneMode: "netstack"`; the non-Linux kernel-driver
stubs are not part of its runtime data path.
