# SPDX-FileCopyrightText: 2022-2026 TII (SSRC) and the Ghaf contributors
# SPDX-License-Identifier: Apache-2.0
{
  config,
  lib,
  pkgs,
  modulesPath,
  ...
}:

let
  roothashPlaceholder = "61fe0f0c98eff2a595dd2f63a5e481a0a25387261fa9e34c37e3a4910edf32b8";
  cfg = config.ghaf.partitioning.verity;
  debugEnable = config.ghaf.profiles.debug.enable;
in
{
  options.ghaf.partitioning.verity = {
    enable = lib.mkEnableOption "the verity (image-based) partitioning scheme";

    split = lib.mkOption {
      description = "Whether to split the partitions to separate files instead of a single image";
      type = lib.types.bool;
      default = true;
    };

    sysupdate = lib.mkOption {
      description = "Enable systemd sysupdate";
      type = lib.types.bool;
      default = true;
    };
  };

  imports = [
    "${modulesPath}/image/repart.nix"
    "${modulesPath}/system/boot/uki.nix"
  ];

  config = lib.mkIf cfg.enable {
    /*
        system.build.ghafImage = config.system.build.image.overrideAttrs (oldAttrs: {
          nativeBuildInputs = oldAttrs.nativeBuildInputs ++ [ pkgs.jq ];
          postInstall = ''
            # Extract the roothash from the JSON
            repartRoothash="$(
              ${lib.getExe pkgs.jq} -r \
                '[.[] | select(.roothash != null)] | .[0].roothash' \
                "$out/repart-output.json"
            )"

            # Replace the placeholder with the real roothash in the target .raw file
            sed -i \
              "0,/${roothashPlaceholder}/ s/${roothashPlaceholder}/$repartRoothash/" \
              "$out/${oldAttrs.pname}_${oldAttrs.version}.raw"

            # Compress the image
            ${pkgs.zstd}/bin/zstd --compress $out/*raw
            rm $out/*raw
          '';
        });
    */
    system.build.ghafImage =
      let
        inherit (config.ghaf) version;
        id = "ghaf";
        fsImage = "$out/${id}_root_${version}.raw";
        verityImage = "$out/${id}_verity_${version}.raw";
        kernelImage = "$out/${id}_kernel_${version}.efi";
        mkfsCommand = "mkfs.erofs -T 1 --all-root -L nix-store --mount-point=/nix/store ${fsImage} --hard-dereference --tar=f";
        regInfo = pkgs.closureInfo {
          rootPaths = [ config.system.build.toplevel ];
        };
      in
      pkgs.runCommandLocal "ghaf-sysupdate-image"
        {
          nativeBuildInputs = [
            pkgs.buildPackages.time
            pkgs.buildPackages.gnutar
            pkgs.buildPackages.erofs-utils
            pkgs.buildPackages.cryptsetup
          ];
          passthru = {
            inherit regInfo;
          };
          __structuredAttrs = true;
          unsafeDiscardReferences.out = true;
        }
        ''
          mkdir $out
          echo Creating a store image
          tar --create \
            --absolute-names \
            --verbatim-files-from \
            --transform 'flags=rSh;s|/nix/store/||' \
            --transform 'flags=rSh;s|~nix~case~hack~[[:digit:]]\+||g' \
            --files-from ${regInfo}/store-paths \
            | time ${mkfsCommand}

          # Align file to block boundary
          truncate -s %4096 ${fsImage}

          echo Creating verity image
          time veritysetup format --root-hash-file $out/dm-verity-root-hash ${fsImage} ${verityImage}
          # Align file to block boundary
          truncate -s %4096 ${verityImage}

          cp ${config.system.build.uki}/${config.system.boot.loader.ukiFile} ${kernelImage}

          # Replace the placeholder with the real roothash in the target .raw file
          verityRoothash=$(cat $out/dm-verity-root-hash)
          sed -i \
            "0,/${roothashPlaceholder}/ s/${roothashPlaceholder}/$verityRoothash/" \
            ${kernelImage}

          # Compress the image
          ${pkgs.zstd}/bin/zstd --compress $out/*raw
          rm -f $out/*raw
        '';

    image.repart.split = cfg.split;

    boot = {
      kernelParams = [
        "storehash=${roothashPlaceholder}"
        "systemd.verity_root_options=panic-on-corruption"
      ]
      ++ lib.optional debugEnable "systemd.setenv=SYSTEMD_SULOGIN_FORCE=1";

      # No bootloaders needed yet
      loader = {
        grub.enable = false;
        systemd-boot.enable = lib.mkForce false;
      };

      # Enable dm-verity and compress initrd
      initrd = {
        systemd = {
          enable = true;
          dmVerity.enable = true;
        };
        nix-store-veritysetup.enable = true;

        compressor = "zstd";
        compressorArgs = [ "-6" ];

        supportedFilesystems = {
          btrfs = true;
          erofs = true;
        };
      };
    };

    environment.systemPackages = with pkgs; [
      cryptsetup
    ];

    # Enable systemd features
    ghaf.systemd = {
      withRepart = true;
      withSysupdate = true;
    };

    # System is now immutable
    system.switch.enable = false;

    # FIXME: merge with definition in repart-common.nix
    swapDevices = [
      {
        device =
          if config.ghaf.storage.encryption.enable then "/dev/mapper/swap" else "/dev/disk/by-partlabel/swap";
        discardPolicy = "both";
        options = [ "nofail" ];
      }
    ];

    fileSystems =
      let
        tmpfsConfig = {
          neededForBoot = true;
          fsType = "tmpfs";
        };
      in
      {
        # FIXME: could we make / a tmpfs, and mount erofs as /nix/store?
        "/" = {
          fsType = "erofs";
          # for systemd-remount-fs
          options = [ "ro" ];
          device = "/dev/mapper/root";
        };
      }
      // builtins.listToAttrs (
        map
          (pathDir: {
            name = pathDir;
            value = tmpfsConfig;
          })
          [
            "/bin" # /bin/sh symlink needs to be created
            "/etc"
            "/home"
            "/root"
            "/tmp"
            "/usr" # /usr/bin/env symlink needs to be created
            "/var"
          ]
      );
  };
}
