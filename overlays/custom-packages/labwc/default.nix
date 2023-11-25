# Copyright 2022-2023 TII (SSRC) and the Ghaf contributors
# SPDX-License-Identifier: Apache-2.0
#
# This overlay customizes labwc - see comments for details
#
(final: prev: {
  labwc = prev.labwc.overrideAttrs (prevAttrs: {
    # Use patch for titlebars. FIXME: make it a patch, not a fork from random point commit
    src = prev.fetchFromGitHub {
      owner = "dmitry-erin";
      repo = "labwc";
      rev = "ba86ef2e8a6ee364062a202aa1e562953edd738f";
      sha256 = "sha256-FA2rzVwFlF1+/pDi1aPneBPmYI6NPe/Pr7zBJVYlsJk=";
    };
    buildInputs = with final;
      [
        foot
        swaybg
        kanshi
        waybar
        mako
        swayidle
      ]
      ++ prevAttrs.buildInputs;
    preInstallPhases = ["preInstallPhase"];
    preInstallPhase = ''
      substituteInPlace ../docs/autostart \
       --replace swaybg ${final.swaybg}/bin/swaybg \
       --replace kanshi ${final.kanshi}/bin/kanshi \
       --replace waybar ${final.waybar}/bin/waybar \
       --replace mako ${final.mako}/bin/mako \
       --replace swayidle ${final.swayidle}/bin/swayidle

       substituteInPlace ../docs/menu.xml \
       --replace alacritty ${final.foot}/bin/foot

       chmod +x ../docs/autostart
    '';
  });
})
