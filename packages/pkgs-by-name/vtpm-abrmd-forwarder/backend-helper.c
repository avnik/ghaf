// SPDX-FileCopyrightText: 2022-2026 TII (SSRC) and the Ghaf contributors
// SPDX-License-Identifier: Apache-2.0

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <tss2/tss2_tcti.h>
#include <unistd.h>

TSS2_RC Tss2_Tcti_Tabrmd_Init(TSS2_TCTI_CONTEXT *tcti_context, size_t *size, const char *conf);

static int read_exact(FILE *f, uint8_t *buf, size_t len) {
  size_t off = 0;
  while (off < len) {
    size_t n = fread(buf + off, 1, len - off, f);
    if (n == 0) {
      if (feof(f)) {
        return -1;
      }
      return -1;
    }
    off += n;
  }
  return 0;
}

static int write_exact(FILE *f, const uint8_t *buf, size_t len) {
  size_t off = 0;
  while (off < len) {
    size_t n = fwrite(buf + off, 1, len - off, f);
    if (n == 0) {
      return -1;
    }
    off += n;
  }
  return fflush(f);
}

int main(int argc, char **argv) {
  if (argc != 2) {
    fprintf(stderr, "usage: %s <backend-device>\n", argv[0]);
    return 2;
  }

  const char *backend = argv[1];
  (void)backend;

  const char *conf = "bus_type=system";

  size_t ctx_size = 0;
  TSS2_RC rc = Tss2_Tcti_Tabrmd_Init(NULL, &ctx_size, conf);
  if (rc != TSS2_RC_SUCCESS || ctx_size == 0) {
    fprintf(stderr, "Tcti_Tabrmd_Init(size) failed rc=0x%08x\n", rc);
    return 1;
  }

  TSS2_TCTI_CONTEXT *ctx = (TSS2_TCTI_CONTEXT *)calloc(1, ctx_size);
  if (!ctx) {
    fprintf(stderr, "calloc failed\n");
    return 1;
  }

  rc = Tss2_Tcti_Tabrmd_Init(ctx, &ctx_size, conf);
  if (rc != TSS2_RC_SUCCESS) {
    fprintf(stderr, "Tcti_Tabrmd_Init failed rc=0x%08x\n", rc);
    free(ctx);
    return 1;
  }

  for (;;) {
    uint8_t lenbuf[4];
    if (read_exact(stdin, lenbuf, sizeof(lenbuf)) != 0) {
      break;
    }

    uint32_t req_len = ((uint32_t)lenbuf[0] << 24) | ((uint32_t)lenbuf[1] << 16) |
                       ((uint32_t)lenbuf[2] << 8) | (uint32_t)lenbuf[3];

    if (req_len == 0 || req_len > 65536) {
      fprintf(stderr, "invalid request length: %u\n", req_len);
      break;
    }

    uint8_t *req = (uint8_t *)malloc(req_len);
    if (!req) {
      fprintf(stderr, "malloc req failed\n");
      break;
    }

    if (read_exact(stdin, req, req_len) != 0) {
      free(req);
      break;
    }

    rc = Tss2_Tcti_Transmit(ctx, req_len, req);
    free(req);
    if (rc != TSS2_RC_SUCCESS) {
      fprintf(stderr, "Transmit failed rc=0x%08x\n", rc);
      break;
    }

    for (;;) {
      size_t cap = 4096;
      uint8_t *resp = (uint8_t *)malloc(cap);
      if (!resp) {
        fprintf(stderr, "malloc resp failed\n");
        free(ctx);
        return 1;
      }

      size_t got = cap;
      rc = Tss2_Tcti_Receive(ctx, &got, resp, TSS2_TCTI_TIMEOUT_BLOCK);
      if (rc == TSS2_TCTI_RC_TRY_AGAIN) {
        usleep(1000 * 10);
        free(resp);
        continue;
      }

      if (rc == TSS2_TCTI_RC_INSUFFICIENT_BUFFER && got > cap && got <= 65536) {
        uint8_t *bigger = (uint8_t *)realloc(resp, got);
        if (!bigger) {
          fprintf(stderr, "realloc resp failed\n");
          free(resp);
          free(ctx);
          return 1;
        }
        resp = bigger;
        cap = got;
        got = cap;
        rc = Tss2_Tcti_Receive(ctx, &got, resp, TSS2_TCTI_TIMEOUT_BLOCK);
        if (rc == TSS2_TCTI_RC_TRY_AGAIN) {
          usleep(1000 * 10);
          free(resp);
          continue;
        }
      }

      if (rc != TSS2_RC_SUCCESS) {
        fprintf(stderr, "Receive failed rc=0x%08x\n", rc);
        free(resp);
        free(ctx);
        return 1;
      }

      if (got == 0 || got > 65536) {
        fprintf(stderr, "invalid response size %zu\n", got);
        free(resp);
        free(ctx);
        return 1;
      }

      uint8_t outlen[4] = {
          (uint8_t)((got >> 24) & 0xff),
          (uint8_t)((got >> 16) & 0xff),
          (uint8_t)((got >> 8) & 0xff),
          (uint8_t)(got & 0xff),
      };
      if (write_exact(stdout, outlen, sizeof(outlen)) != 0 || write_exact(stdout, resp, got) != 0) {
        free(resp);
        free(ctx);
        return 1;
      }

      free(resp);
      break;
    }
  }

  free(ctx);
  return 0;
}
