// SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause)
/* Copyright (c) 2022 Hengqi Chen */

#ifndef __BUFFER_BPF_H
#define __BUFFER_BPF_H

#include <vmlinux.h>
#include <bpf/bpf_helpers.h>

#ifndef MAX_EVENT_SIZE
#define MAX_EVENT_SIZE		10240
#endif

#define GADGET_TRACER_MAP(name, size)			\
	struct {					\
		__uint(type, BPF_MAP_TYPE_RINGBUF);	\
		__uint(max_entries, size);		\
	} name SEC(".maps");				\
	const void *gadget_map_tracer_##name __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__uint(key_size, sizeof(__u32));
	__uint(value_size, MAX_EVENT_SIZE);
} gadget_heap SEC(".maps");

static __always_inline void *gadget_reserve_buf(void *map, __u64 size)
{
	static const int zero = 0;

	if (bpf_core_type_exists(struct bpf_ringbuf))
		return bpf_ringbuf_reserve(map, size, 0);

	return bpf_map_lookup_elem(&gadget_heap, &zero);
}

static __always_inline long gadget_submit_buf(void *ctx, void *map, void *buf, __u64 size)
{
	if (bpf_core_type_exists(struct bpf_ringbuf)) {
		bpf_ringbuf_submit(buf, 0);
		return 0;
	}

	return bpf_perf_event_output(ctx, map, BPF_F_CURRENT_CPU, buf, size);
}

#endif /* __BUFFER_BPF_H */
