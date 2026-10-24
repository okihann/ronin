---
name: rom-porting
description: Port an Android ROM from a donor device to a target device - inspect images, diff vendor blobs, check compatibility, plan the port.
---

# Android ROM porting

Read-only analysis for porting a ROM built for a donor device onto a
target phone. Nothing here flashes, writes to a device, or repacks a
bootable image; those steps are the user's, on their own hardware.

## Before anything else: establish the two devices

Porting succeeds or fails on device similarity. Collect these for both
donor and target before touching files:

    ro.product.device      ro.product.model      ro.board.platform
    ro.product.cpu.abi     ro.build.version.sdk  ro.treble.enabled
    ro.vendor.build.version.sdk                  ro.soc.manufacturer
    ro.boot.dynamic_partitions                   ro.build.ab_update

Get them from the ROM's build.prop files, or on a booted device with
`getprop`. Ask the user for anything not on disk; do not guess.

The compatibility rules, in order of how fatal they are:

1. **Same SoC family is effectively required.** Snapdragon to Snapdragon,
   MTK to MTK. Vendor HALs are closed binaries built against that SoC's
   kernel interfaces. A cross-SoC port is a research project, not a port.
2. **Same or forward-compatible Android version.** Porting to an older
   target usually fails on HAL version mismatches.
3. **Same ABI.** arm64-v8a to arm64-v8a. vendordiff.py reports
   cross-architecture blobs; any hit there is a hard stop.
4. **Treble status matters.** If both are Treble, you can often reuse the
   target's vendor partition and only port system. If the target is
   non-Treble, you are porting the whole thing and it is much harder.

Say plainly which of these fails rather than proceeding.

## Inspecting the images

Run the bundled script on every image before extracting anything:

    python3 scripts/rominfo.py super.img
    python3 scripts/rominfo.py system.img --json

It reports sparse format, ext4 superblock (label, UUID, block size,
64-bit, metadata_csum), AVB footer, and super.img logical partition
layout. Notes:

- **Extracting needs `simg2img` first** for sparse images. That tool is
  not installed here; tell the user to install `android-sdk-libsparse-utils`
  rather than trying to hand-roll it.
- **`lpunpack` is likewise missing** for super.img. Same advice.
- **`debugfs` is present** and can pull individual files out of an ext4
  image read-only: `debugfs -R "dump /system/build.prop /tmp/bp" system.img`
- Do not claim an image was unpacked if the tool was missing. Report it.

## The vendor diff

Once vendor trees are extracted:

    python3 scripts/vendordiff.py target_vendor/ donor_vendor/

Read the output as three separate questions:

- **only_in_target** - files the target firmware expects that the donor
  does not ship. These are the ones that break boot: a missing
  `libqti-perfd-client.so` means an init service fails, not a cosmetic gap.
- **only_in_donor** - usually safe to ignore; the donor ships extras.
- **arch_mismatch** - always fatal. Report it and stop.

Size differences on the same path mean a genuinely different build. Do not
assume the donor's version is compatible just because the name matches.

## The port plan

Produce a plan, not a build. Ordered:

1. State the compatibility verdict from the SoC/Treble/ABI checks.
2. List what is being kept from the target (usually vendor + kernel) and
   what comes from the donor (usually system).
3. Name the specific missing blobs from vendordiff, by path.
4. List the props that must be patched, and why - typically
   `ro.product.device`, fingerprint, and any `ro.vendor.*` that encode the
   donor device.
5. State what cannot be predicted and will need on-device iteration.

## What an agent cannot do here

Be explicit about this rather than implying otherwise:

- **It cannot make the ROM boot.** Boot failures are diagnosed by reading
  `dmesg`, `logcat`, and `last_kmsg` after a flash attempt, then patching
  and reflashing. That loop is physical.
- **It cannot verify HAL compatibility** without the target's vendor
  partition contents and a running device.
- **It cannot flash anything**, and should refuse if asked to try.

The useful role is narrowing the search space before a flash: finding the
missing blob, the mismatched ABI, the wrong prop, the partition that is
too small.

## Failure modes worth checking first

When the user reports a specific failure:

- **Bootloop before animation** - kernel or vendor mismatch; check that
  the boot.img kernel matches the SoC, and that vbmeta was disabled.
- **Bootloop at animation** - system/vendor prop mismatch or a missing
  HAL; `logcat` from recovery.
- **No display** - vendor display HAL mismatch.
- **No modem/SIM** - modem firmware and vendor RIL must both match the
  target's baseband; these are almost never portable across devices.
- **Camera** - the most device-specific subsystem; expect to port the
  target's camera HAL, not the donor's.
