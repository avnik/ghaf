(final: prev: {
  jbig2dec = prev.jbig2dec.overrideAttrs (oa: {
    # Strip ./configure invocation from autogen (which create some required files), and allow default mechanism to call ./configure with all required arguments
    preConfigure = ''
      sed -i '$d' autogen.sh;
    '' + oa.preConfigure;
  });
})
