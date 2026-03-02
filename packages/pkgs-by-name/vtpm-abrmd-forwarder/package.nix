# SPDX-FileCopyrightText: 2022-2026 TII (SSRC) and the Ghaf contributors
# SPDX-License-Identifier: Apache-2.0
{
  lib,
  stdenv,
  go,
  makeWrapper,
  pkg-config,
  tpm2-tss,
  tpm2-abrmd,
}:
stdenv.mkDerivation {
  pname = "vtpm-abrmd-forwarder";
  version = "0.1.0";

  src = ./.;

  nativeBuildInputs = [
    go
    makeWrapper
    pkg-config
  ];

  buildInputs = [
    tpm2-tss
    tpm2-abrmd
  ];

  buildPhase = ''
    runHook preBuild
    export HOME="$TMPDIR"
    export GOCACHE="$TMPDIR/go-cache"
    export GO111MODULE=on
    cc -O2 -Wall -Wextra -o vtpm-tcti-device-helper-bin ./backend-helper.c $(pkg-config --cflags --libs tss2-tcti-tabrmd)
    go build -trimpath -o vtpm-abrmd-forwarder \
      ./main.go \
      ./backend_helper.go \
      ./systemd_notify.go \
      ./tpm_proto.go \
      ./vtpm_proxy.go
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p "$out/bin"
    install -m0755 vtpm-abrmd-forwarder "$out/bin/vtpm-abrmd-forwarder"
    install -m0755 vtpm-tcti-device-helper-bin "$out/bin/vtpm-tcti-device-helper-bin"
    makeWrapper "$out/bin/vtpm-tcti-device-helper-bin" "$out/bin/vtpm-tcti-device-helper" \
      --prefix LD_LIBRARY_PATH : "${
        lib.makeLibraryPath [
          tpm2-abrmd
          tpm2-tss
        ]
      }"
    runHook postInstall
  '';

  meta = {
    description = "Per-VM TPM forwarder scaffold for abrmd muxing";
    mainProgram = "vtpm-abrmd-forwarder";
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
    license = lib.licenses.asl20;
  };
}
