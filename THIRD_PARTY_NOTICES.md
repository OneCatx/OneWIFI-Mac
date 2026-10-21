# Third-Party Notices

## Rust userspace IP stack

The native VoWiFi IP-device integration uses smoltcp 0.14.0
(https://github.com/smoltcp-rs/smoltcp), with its 0BSD license:

```text
Copyright (C) smoltcp contributors

Permission to use, copy, modify, and/or distribute this software for
any purpose with or without fee is hereby granted.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN
AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

OneWIFI for Mac contains code derived from the upstream VoHive project and retains the license and required notice provided in the repository root [`LICENSE`](LICENSE):

```text
Required Notice: Copyright iniwex5 (https://github.com/iniwex5/vohive)
```

The call-mode USB/ADB design was also informed by the PolyForm Noncommercial reference repository [`rogerbush007-a11y/DJOneHub-mac-enhanced`](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced), inspected at revision `f6ee6aa5ba5d1ca13ef65956adda7bc098c2fb92`. It carries the same upstream required notice and is not embedded as a binary dependency.

Legacy `QADBKEY` interoperability was informed by the public GPL-3.0-licensed [`carp4/qadbkey-unlock`](https://github.com/carp4/qadbkey-unlock) reference at revision `cab52a0a7429c8d8b8f31da8894c8c93155c0fc5` and independently implemented in Go using the documented Unix MD5-crypt format. OneWIFI does not bundle or execute that Python script; the compatibility implementation derives the password locally in memory and never persists it.

## ESTKme-RED BLE protocol reference

The ESTKme-RED frame format and GATT UUIDs in `backend/internal/estkble` and the macOS Bluetooth discovery UI were adapted from [NekokoLPA](https://github.com/iebb/NekokoLPA), by Kosmoneko OÜ (2024) and Infineon Technologies AG (2022), under the MIT license. Its required copyright and permission notice is retained below:

```text
Copyright (c) 2024 Kosmoneko OÜ
Copyright (c) 2022 Infineon Technologies AG

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.
```

The upstream source is not bundled as an executable dependency.

## NekokoLPA2 profile-size reference data

`backend/internal/esim/data/reference_sizes_simple.json` is derived from
NekokoLPA2 revision `013eadad7037248531f1b1532ebf344390d57087` and is used
only to display a fallback estimated profile size when the SM-DP+ metadata
does not include one. The data is distributed under the MIT license:

```text
Copyright (c) 2026 Nekoko

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## Release Runtime

The macOS release package includes **libusb 1.0.30**, distributed under the GNU Lesser General Public License, version 2.1 or later.

- Project: <https://libusb.info/>
- Source: <https://github.com/libusb/libusb/releases/tag/v1.0.30>
- License text in the release package: `licenses/libusb-COPYING`

## Optional Module Voice Runtime

OneWIFI does **not** include the QDC507 module voice runtime in its source tree or release package. When a user explicitly confirms the download in the call page, the application downloads the files from an immutable MaVo revision, either directly from GitHub or through a user-selected HTTPS relay, then verifies fixed file sizes and SHA-256 hashes before saving or deploying anything.

- Project: <https://github.com/moluncn/mavo>
- Exact source revision: `0443dfdaf8aec086fd76ba2ee9152fd908114524`
- Upstream runtime directory: <https://github.com/moluncn/mavo/tree/0443dfdaf8aec086fd76ba2ee9152fd908114524/Resources/ModuleVoice>
- Local download directory: `~/Library/Application Support/OneWIFI/voice-runtime/qdc507-3.18.44-voice-20260712.5`
- Downloaded license copy: `COPYING-GPL-2.0`
- Downloaded build/safety report: `MODULE-REPORT.md`

The downloaded helper `mavo-pcm-bridge.armv7` is part of the MIT-licensed MaVo project. The two downloaded kernel objects expose `MODULE_LICENSE("GPL v2")` metadata and are accompanied upstream by the GPLv2 text. OneWIFI neither mirrors nor republishes these artifacts; relay services are transport alternatives and are not treated as trusted publishers. This notice describes the application behavior and does not replace the upstream license materials.

## Vendored Source Dependencies

The source repository includes selected dependencies under `backend/third_party/` so the platform adaptations and narrowly scoped patches remain reproducible. Their original copyright notices and license texts are retained in the corresponding directories and copied into the app's `Contents/Resources/licenses/` directory.

| Component | License file |
| --- | --- |
| euicc-go | `backend/third_party/euicc-go/LICENSE` |
| euicc-go BER-TLV | `backend/third_party/euicc-go/bertlv/LICENSE` |
| uicc-go | `backend/third_party/uicc-go/LICENSE` |
| quectel-qmi-go | `backend/third_party/quectel-qmi-go/LICENSE` |
| swu-go | `backend/third_party/swu-go/LICENSE` |
| opencore-amr | `backend/third_party/opencore-amr/LICENSE` and `opencore/NOTICE` |

euicc-go remains pinned to `v1.1.3-0.20260628013808-8d873a2dfc98`; the STORE DATA framing correction and upstream hashes are documented in `backend/third_party/euicc-go/UPSTREAM.md`. uicc-go remains pinned to `v0.0.0-20260629073618-7ddada6bb13e`; the local PC/SC reset addition is documented in `backend/third_party/uicc-go/UPSTREAM.md`. The eUICC interoperability review used NekokoLPA2 revision `013eadad7037248531f1b1532ebf344390d57087` as a behavioral reference; its Flutter runtime and art assets are not embedded.

Dependencies fetched through Go modules retain their own licenses and copyright notices. This file is informational and does not replace any component's full license text.

## RustCrypto CBC and block-padding

cbc 0.2.1 and block-padding 0.4.2 are used under the MIT license option.

Copyright (c) 2018-2022 RustCrypto Developers
Copyright (c) 2018 Artyom Pavlov
Copyright (c) 2018-2025 The RustCrypto Project Developers

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
