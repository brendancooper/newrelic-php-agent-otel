/*
 * Copyright 2020 New Relic Corporation. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "nr_guid.h"

#include "util_memory.h"

static const char* hex_digits = "0123456789abcdef";

char* nr_guid_create(nr_random_t* rnd) {
  char* guid = nr_zalloc(NR_GUID_SIZE + 1);
  size_t i;

  for (i = 0; i < NR_GUID_SIZE; i++) {
    unsigned long r = nr_random_range(rnd, 0xf);

    guid[i] = hex_digits[r];
  }

  return guid;
}

char* nr_trace_id_create(nr_random_t* rnd) {
  char* tid = nr_zalloc(NR_TRACE_ID_SIZE + 1);
  size_t i;
  int any_nonzero = 0;

  for (i = 0; i < NR_TRACE_ID_SIZE; i++) {
    unsigned long r = nr_random_range(rnd, 0xf);

    tid[i] = hex_digits[r];
    if (r != 0) {
      any_nonzero = 1;
    }
  }

  /*
   * The all-zero trace id is reserved by the W3C Trace Context spec. The
   * probability of generating it is 16^-32, but defensively flip the final
   * digit to '1' if it occurs.
   */
  if (!any_nonzero) {
    tid[NR_TRACE_ID_SIZE - 1] = '1';
  }

  /*
   * Also avoid the leading 16 hex characters being all zero (the OTLP
   * exporter left-pads legacy 16-hex trace ids to 32 hex with leading zeros;
   * a freshly-generated root id shouldn't look like a padded legacy id, so
   * ensure at least one set bit in the upper 64 bits).
   */
  for (i = 0; i < NR_GUID_SIZE; i++) {
    if (tid[i] != '0') {
      return tid;
    }
  }
  /* All zeros in the upper half: bump the first character to a nonzero hex. */
  tid[0] = hex_digits[nr_random_range(rnd, 0xf) + 1]; /* 0x1..0xf */
  return tid;
}
