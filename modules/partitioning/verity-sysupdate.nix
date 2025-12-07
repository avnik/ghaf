# SPDX-FileCopyrightText: 2022-2026 TII (SSRC) and the Ghaf contributors
# SPDX-License-Identifier: Apache-2.0
{
  config,
  lib,
  pkgs,
  ...
}:
let
  url = "/persist/sysupdate";
  id = "ghaf";
  cfg = config.ghaf.partitioning.verity;
in
{
  config = lib.mkIf cfg.sysupdate {
    ghaf.systemd.withSysupdate = true;
    ghaf.systemd.withMachines = true;
    # TODO: This is a placeholder for future implementation.
    systemd.sysupdate = {
      enable = true;
      reboot.enable = false; # FIXME: no auto-reboot (at least temporary)
      transfers = {
        # FIXME: update systemd boot as well
        "10-uki" = {
          Transfer = {
            Verify = "no";
          };
          Source = {
            Type = "regular-file";
            Path = url;
            MatchPattern = "${id}_kernel_@v.efi";
          };
          Target = {
            Type = "regular-file";
            Path = "/EFI/Linux";
            PathRelativeTo = "esp";
            MatchPattern = "${config.boot.uki.name}_@v+@l-@d.efi ${config.boot.uki.name}_@v+@l.efi ${config.boot.uki.name}_@v.efi";
            Mode = "0444";
            TriesLeft = 3;
            TriesDone = 0;
            InstancesMax = 2;
          };
        };
        "20-root-verity" = {
          Transfer = {
            Verify = "no";
          };
          Source = {
            Type = "regular-file";
            Path = url;
            MatchPattern = "${id}_verity_@v.raw.zst";
          };
          Target = {
            Type = "partition";
            Path = "auto";
            MatchPattern = "root-verity_@v";
            MatchPartitionType = "root-verity";
            ReadOnly = 1;
          };
        };
        "22-root" = {
          Transfer = {
            Verify = "no";
          };
          Source = {
            Type = "regular-file";
            Path = url;
            MatchPattern = "${id}_root_@v.raw.zst";
          };
          Target = {
            Type = "partition";
            Path = "auto";
            MatchPattern = "root_@v";
            MatchPartitionType = "root";
            ReadOnly = 1;
          };
        };
      };
    };
    environment.systemPackages = [
      # this is only for running `systemd-sysupdate vacuum -m 1` becasue
      # `updatectl vacuum` does not support the parameter `-m 1`
      (pkgs.runCommand "systemd-extratools" { } ''
        mkdir -p $out/bin
        ln -s ${config.systemd.package}/lib/systemd-sysupdate $out/bin
      '')
    ];
  };
}
