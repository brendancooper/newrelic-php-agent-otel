/*
 * Copyright 2020 New Relic Corporation. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef NR_GUID_HDR
#define NR_GUID_HDR

#include "util_random.h"

/*
 * The guid is an identifier that is unique to the transaction and is used
 * to tie this transaction trace to a browser trace and/or external
 * (cross application) traces. NR GUIDs are 16 hex characters (64-bit).
 */
#define NR_GUID_SIZE 16

/*
 * A W3C trace id is 128-bit (32 lowercase hex characters). See W3C Trace
 * Context 3.4. The agent generates a fresh random trace id at the start of
 * a root transaction when otel_w3c_trace_id is enabled (see php_nrini.c);
 * the agent already shortens / zero-pads legacy 16-hex trace ids when
 * emitting them in traceparent headers, but for non-PHP trace correlation
 * and OTLP /v1/traces egress we want a real 128-bit id rather than a
 * left-padded 64-bit one.
 */
#define NR_TRACE_ID_SIZE 32

/*
 * Purpose : Create a new GUID.
 *
 * Params  : 1. The random number generator to use.
 *
 * Returns : A newly allocated, null terminated string, which is owned by the
 *           caller.
 */
extern char* nr_guid_create(nr_random_t* rnd);

/*
 * Purpose : Create a new W3C 128-bit trace id (32 lowercase hex chars). The
 *           id is uniformly random except that the all-zero id (which is
 *           reserved by the W3C spec) is never produced.
 *
 * Params  : 1. The random number generator to use.
 *
 * Returns : A newly allocated, null terminated string of NR_TRACE_ID_SIZE
 *           lowercase hex characters, owned by the caller.
 */
extern char* nr_trace_id_create(nr_random_t* rnd);

#endif /* NR_GUID_HDR */
