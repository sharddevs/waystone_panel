# Font files

IBM Plex, from IBM's official npm releases of github.com/IBM/plex. They are copied unchanged from each package's `fonts/complete/woff2/`. The licence is the SIL Open Font License 1.1 (OFL.txt here, copied from the packages' LICENSE.txt). A test (src/fonts.test.ts) checks every sha256 below.

| Package | Integrity (npm dist.integrity) |
|---|---|
| @ibm/plex-sans 1.1.0 | sha512-WPgvO6Yfj2w5YbhyAr1tv95RUz4LRJlqN+CmYvBglabXteufP1D1E9BABMde+ZIKdRbFJDoKF5eQzfhpnbgZcQ== |
| @ibm/plex-mono 2.5.0 | sha512-STBJIPxPomOYPmBMO7z5TKPJUotAF9u3gAUumTqVgwgrAO+K4FRNh0MlhsoJjKhJKsMbBJR10/bk4inkj/wc1w== |

| File | sha256 |
|---|---|
| IBMPlexSans-Regular.woff2 | ba711a3085ff9f27440b6b9c4550cfc47c97bf36591d5da958b975bb3add8c1a |
| IBMPlexSans-Medium.woff2 | 5660f8a658f8bb50dbc005232f885eadffd2bc1c235c4f6fbb63469d1f9cde6d |
| IBMPlexSans-SemiBold.woff2 | f78048030eab62e860efa39a0df79e2e5581bf122eb95b9bc42c0b8a4988d205 |
| IBMPlexMono-Regular.woff2 | ba204497f16b6d334cee9d1e963a831b73e3a56e1d6300a8489d18df7214b350 |
| IBMPlexMono-Medium.woff2 | 33faf307fa6031fb4062276d7320a6d632de890cbb347576fd80cfa01077bc25 |
| OFL.txt | 7e6b2818edbd8f6a01ae80641cc8f16a51080d08fb4e532be3a0b6f74adb07da |

To update, run `npm pack @ibm/plex-sans@<v> @ibm/plex-mono@<v>`, check the tarballs' integrity against `npm view <pkg>@<v> dist.integrity`, copy the five files and the licence, then update both tables and the test.
