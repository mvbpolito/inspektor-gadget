// Copyright 2023 The Inspektor Gadget authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !withoutebpf

package tracer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/exp/constraints"

	log "github.com/sirupsen/logrus"

	containercollection "github.com/inspektor-gadget/inspektor-gadget/pkg/container-collection"
	gadgetcontext "github.com/inspektor-gadget/inspektor-gadget/pkg/gadget-context"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets/internal/networktracer"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets/internal/socketenricher"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets/run/types"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/netnsenter"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/params"
	eventtypes "github.com/inspektor-gadget/inspektor-gadget/pkg/types"
	bpfiterns "github.com/inspektor-gadget/inspektor-gadget/pkg/utils/bpf-iter-ns"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/utils/experimental"
)

// keep aligned with pkg/gadgets/common/types.h
type l3EndpointT struct {
	addr    [16]byte
	version uint8
	pad     [3]uint8 // manual padding to avoid issues between C and Go
}

type l4EndpointT struct {
	l3    l3EndpointT
	port  uint16
	proto uint16
}

type Config struct {
	ProgContent []byte
	Metadata    *types.GadgetMetadata
	MountnsMap  *ebpf.Map

	// constants to replace in the ebpf program
	Consts map[string]interface{}
}

type linkSnapshotter struct {
	link *link.Iter
	typ  string
}

type Tracer struct {
	config             *Config
	eventCallback      func(*types.Event)
	eventArrayCallback func([]*types.Event)
	mu                 sync.Mutex

	spec       *ebpf.CollectionSpec
	collection *ebpf.Collection
	// Type describing the format the gadget uses
	eventType *btf.Struct

	socketEnricher *socketenricher.SocketEnricher
	networkTracers map[string]*networktracer.Tracer[types.Event]

	// Tracers related
	ringbufReader *ringbuf.Reader
	perfReader    *perf.Reader

	// Snapshotters related
	linksSnapshotters []*linkSnapshotter

	containers map[string]*containercollection.Container
	links      []link.Link

	eventFactory *types.EventFactory
}

func (g *GadgetDesc) NewInstance() (gadgets.Gadget, error) {
	return &Tracer{}, nil
}

func (t *Tracer) Init(gadgetCtx gadgets.GadgetContext) error {
	if !experimental.Enabled() {
		return errors.New("run needs experimental features to be enabled")
	}

	t.config = &Config{}
	t.containers = make(map[string]*containercollection.Container)
	t.networkTracers = make(map[string]*networktracer.Tracer[types.Event])

	params := gadgetCtx.GadgetParams()
	args := gadgetCtx.Args()

	pullSecretString := params.Get(pullSecret).AsString()
	var secretBytes []byte
	if pullSecretString != "" {
		var err error
		// TODO: Namespace is still hardcoded
		secretBytes, err = getPullSecret(pullSecretString, "gadget")
		if err != nil {
			return err
		}
	}

	info, err := getGadgetInfo(params, args, secretBytes, gadgetCtx.Logger())
	if err != nil {
		return fmt.Errorf("getting gadget info: %w", err)
	}

	t.eventFactory = info.EventFactory
	t.config.ProgContent = info.ProgContent
	t.spec, err = loadSpec(t.config.ProgContent)
	if err != nil {
		return err
	}

	t.config.Metadata = info.GadgetMetadata

	// Create network tracers, one for each socket filter program.
	// We need to make this in Init() because AttachContainer() is called before Run().
	for _, p := range t.spec.Programs {
		if p.Type == ebpf.SocketFilter && strings.HasPrefix(p.SectionName, "socket") {
			networkTracer, err := networktracer.NewTracer[types.Event]()
			if err != nil {
				t.Close()
				return fmt.Errorf("creating network tracer: %w", err)
			}
			t.networkTracers[p.Name] = networkTracer
		}
	}

	return nil
}

func (t *Tracer) Close() {
	if t.collection != nil {
		t.collection.Close()
		t.collection = nil
	}
	for _, l := range t.links {
		gadgets.CloseLink(l)
	}
	t.links = nil

	if t.ringbufReader != nil {
		t.ringbufReader.Close()
	}
	if t.perfReader != nil {
		t.perfReader.Close()
	}
	if t.socketEnricher != nil {
		t.socketEnricher.Close()
	}
	for _, networkTracer := range t.networkTracers {
		networkTracer.Close()
	}
}

var (
	onceRingbuf      sync.Once
	ringbufAvailable bool
)

func isRingbufAvailable() bool {
	onceRingbuf.Do(func() {
		ringbuf, err := ebpf.NewMap(&ebpf.MapSpec{
			Type:       ebpf.RingBuf,
			MaxEntries: uint32(os.Getpagesize()),
		})

		ringbuf.Close()

		ringbufAvailable = err == nil
	})

	return ringbufAvailable
}

// createdByTracerMapMacro returns whether the tracer map was created using
// GADGET_TRACER_MAP().
func (t *Tracer) createdByTracerMapMacro(tracerMapName string) bool {
	results, err := types.GetGadgetIdentByPrefix(t.spec, types.TracerMapPrefix)
	if err != nil {
		return false
	}

	return slices.Contains(results, tracerMapName)
}

func (t *Tracer) handleTracerMapDefinition(tracerMapName string) error {
	if !isRingbufAvailable() {
		if tracerMapName != "" {
			log.Debugf("Ring buffers are not available, defaulting to perf ones")

			bufMap, ok := t.spec.Maps[tracerMapName]
			if !ok {
				return fmt.Errorf("no buffer map named %s", tracerMapName)
			}

			bufMap.Type = ebpf.PerfEventArray
			bufMap.KeySize = 4
			bufMap.ValueSize = 4

			t.spec.Maps[tracerMapName] = bufMap
		}
	} else {
		heapMapName := "gadget_heap"
		// If we delete the gadget_heap map, the verifier will not load the code
		// because it cannot find it despite the code using it being dead one:
		// ...: create BPF collection: program ig_mount_x: missing map gadget_heap
		// Rather than deleting it, we just set its size to 0 and use a hash to
		// avoid having one struct per CPU, i.e. reducing as much as possible the
		// memory footprint it uses.
		heapMap, ok := t.spec.Maps[heapMapName]
		if ok {
			heapMap.Type = ebpf.Hash
			heapMap.ValueSize = 4

			t.spec.Maps[heapMapName] = heapMap
		}
	}

	return nil
}

type loadingOptions struct {
	collectionOptions ebpf.CollectionOptions
	tracerMapName     string
}

func (t *Tracer) loadeBPFObjects(opts loadingOptions) error {
	var err error

	tracerMapName := opts.tracerMapName

	if t.createdByTracerMapMacro(tracerMapName) {
		err = t.handleTracerMapDefinition(tracerMapName)
		if err != nil {
			return fmt.Errorf("handling tracer map definition through GADGET_TRACER_MAP: %w", err)
		}
	}

	gadgets.FixBpfKtimeGetBootNs(t.spec.Programs)

	t.collection, err = ebpf.NewCollectionWithOptions(t.spec, opts.collectionOptions)
	if err != nil {
		return fmt.Errorf("create BPF collection: %w", err)
	}

	// Some logic before loading the programs
	if tracerMapName != "" {
		m := t.collection.Maps[tracerMapName]
		switch m.Type() {
		case ebpf.RingBuf:
			t.ringbufReader, err = ringbuf.NewReader(t.collection.Maps[tracerMapName])
		case ebpf.PerfEventArray:
			t.perfReader, err = perf.NewReader(t.collection.Maps[tracerMapName], gadgets.PerfBufferPages*os.Getpagesize())
		}
		if err != nil {
			return fmt.Errorf("create BPF map reader: %w", err)
		}
	}

	return err
}

func (t *Tracer) handleTracers() (string, error) {
	_, tracer := getAnyMapElem(t.config.Metadata.Tracers)

	traceMap := t.spec.Maps[tracer.MapName]
	if traceMap == nil {
		return "", fmt.Errorf("map %q not found", tracer.MapName)
	}

	return tracer.MapName, nil
}

func (t *Tracer) attachProgram(gadgetCtx gadgets.GadgetContext, p *ebpf.ProgramSpec, prog *ebpf.Program) (link.Link, error) {
	logger := gadgetCtx.Logger()

	switch p.Type {
	case ebpf.Kprobe:
		switch {
		case strings.HasPrefix(p.SectionName, "kprobe/"):
			logger.Debugf("Attaching kprobe %q to %q", p.Name, p.AttachTo)
			return link.Kprobe(p.AttachTo, prog, nil)
		case strings.HasPrefix(p.SectionName, "kretprobe/"):
			logger.Debugf("Attaching kretprobe %q to %q", p.Name, p.AttachTo)
			return link.Kretprobe(p.AttachTo, prog, nil)
		}
		return nil, fmt.Errorf("unsupported section name %q for program %q", p.Name, p.SectionName)
	case ebpf.TracePoint:
		logger.Debugf("Attaching tracepoint %q to %q", p.Name, p.AttachTo)
		parts := strings.Split(p.AttachTo, "/")
		return link.Tracepoint(parts[0], parts[1], prog, nil)
	case ebpf.SocketFilter:
		logger.Debugf("Attaching socket filter %q to %q", p.Name, p.AttachTo)
		networkTracer := t.networkTracers[p.Name]
		return nil, networkTracer.AttachProg(prog)
	case ebpf.Tracing:
		switch {
		case strings.HasPrefix(p.SectionName, "iter/"):
			logger.Debugf("Attaching iter %q to %q", p.Name, p.AttachTo)
			switch p.AttachTo {
			case "task", "tcp", "udp":
				return link.AttachIter(link.IterOptions{
					Program: prog,
				})
			}
			return nil, fmt.Errorf("unsupported iter type %q", p.AttachTo)
		case strings.HasPrefix(p.SectionName, "fentry/"):
			logger.Debugf("Attaching fentry %q to %q", p.Name, p.AttachTo)
			return link.AttachTracing(link.TracingOptions{
				Program:    prog,
				AttachType: ebpf.AttachTraceFEntry,
			})
		case strings.HasPrefix(p.SectionName, "fexit/"):
			logger.Debugf("Attaching fexit %q to %q", p.Name, p.AttachTo)
			return link.AttachTracing(link.TracingOptions{
				Program:    prog,
				AttachType: ebpf.AttachTraceFExit,
			})
		}
		return nil, fmt.Errorf("unsupported section name %q for program %q", p.Name, p.SectionName)
	case ebpf.RawTracepoint:
		logger.Debugf("Attaching raw tracepoint %q to %q", p.Name, p.AttachTo)
		return link.AttachRawTracepoint(link.RawTracepointOptions{
			Name:    p.AttachTo,
			Program: prog,
		})
	}

	return nil, fmt.Errorf("unsupported program %q of type %q", p.Name, p.Type)
}

func (t *Tracer) installTracer(gadgetCtx gadgets.GadgetContext) error {
	params := gadgetCtx.GadgetParams()

	var err error
	var tracerMapName string

	mapReplacements := map[string]*ebpf.Map{}

	t.eventType, err = getEventTypeBTF(t.config.ProgContent, t.config.Metadata)
	if err != nil {
		return err
	}

	switch {
	case len(t.config.Metadata.Tracers) > 0:
		tracerMapName, err = t.handleTracers()
		if err != nil {
			return fmt.Errorf("handling trace programs: %w", err)
		}
	}

	t.setEBPFParameters(t.config.Metadata.EBPFParams, params)
	consts := t.config.Consts

	// Handle special maps like mount ns filter, socket enricher, etc.
	for _, m := range t.spec.Maps {
		switch m.Name {
		// Only create socket enricher if this is used by the tracer
		case socketenricher.SocketsMapName:
			t.socketEnricher, err = socketenricher.NewSocketEnricher()
			if err != nil {
				// Containerized gadgets require a kernel with BTF
				return fmt.Errorf("creating socket enricher: %w", err)
			}
			mapReplacements[socketenricher.SocketsMapName] = t.socketEnricher.SocketsMap()
		// Replace filter mount ns map
		case gadgets.MntNsFilterMapName:
			if t.config.MountnsMap == nil {
				break
			}

			mapReplacements[gadgets.MntNsFilterMapName] = t.config.MountnsMap
			consts[gadgets.FilterByMntNsName] = true
		}
	}

	if err := t.spec.RewriteConstants(consts); err != nil {
		return fmt.Errorf("rewriting constants: %w", err)
	}

	// Load the ebpf objects
	err = t.loadeBPFObjects(loadingOptions{
		collectionOptions: ebpf.CollectionOptions{MapReplacements: mapReplacements},
		tracerMapName:     tracerMapName,
	})
	if err != nil {
		return fmt.Errorf("loading eBPF objects: %w", err)
	}

	// Attach programs
	for progName, p := range t.spec.Programs {
		l, err := t.attachProgram(gadgetCtx, p, t.collection.Programs[progName])
		if err != nil {
			return fmt.Errorf("attaching eBPF program %q: %w", progName, err)
		}
		if l != nil {
			t.links = append(t.links, l)
		}

		// we need to store links to iterators on a separated list because we need them to run the programs.
		if p.Type == ebpf.Tracing && strings.HasPrefix(p.SectionName, "iter/") {
			lIter, ok := l.(*link.Iter)
			if !ok {
				return fmt.Errorf("link is not an iterator")
			}
			t.linksSnapshotters = append(t.linksSnapshotters, &linkSnapshotter{link: lIter, typ: p.AttachTo})
		}
	}

	return nil
}

func verifyGadgetUint64Typedef(t btf.Type) error {
	typDef, ok := t.(*btf.Typedef)
	if !ok {
		return fmt.Errorf("not a typedef")
	}

	underlying, err := getUnderlyingType(typDef)
	if err != nil {
		return err
	}

	intM, ok := underlying.(*btf.Int)
	if !ok {
		return fmt.Errorf("not an integer")
	}

	if intM.Size != 8 {
		return fmt.Errorf("bad sized. Expected 8, got %d", intM.Size)
	}

	return nil
}

func getAsInteger[OT constraints.Integer](data []byte, offset uint32) OT {
	return *(*OT)(unsafe.Pointer(&data[offset]))
}

// processEventFunc returns a callback that parses a binary encoded event in data, enriches and
// returns it.
func (t *Tracer) processEventFunc(gadgetCtx gadgets.GadgetContext) func(data []byte) *types.Event {
	typ := t.eventType
	logger := gadgetCtx.Logger()

	var mntNsIdstart uint32
	mountNsIdFound := false

	type endpointType int

	const (
		U endpointType = iota
		L3
		L4
	)

	type endpointDef struct {
		name  string
		start uint32
		typ   endpointType
	}

	endpointDefs := []endpointDef{}
	timestampsOffsets := []uint32{}

	enumSetters := []func(ev *types.Event, data []byte){}

	// The same same data structure is always sent, so we can precalculate the offsets for
	// different fields like mount ns id, endpoints, etc.
	for _, member := range typ.Members {
		member := member
		switch member.Type.TypeName() {
		case types.MntNsIdTypeName:
			if err := verifyGadgetUint64Typedef(member.Type); err != nil {
				logger.Warn("%s is not a uint64: %s", member.Name, err)
				continue
			}
			mntNsIdstart = member.Offset.Bytes()
			mountNsIdFound = true
		case types.L3EndpointTypeName:
			typ, ok := member.Type.(*btf.Struct)
			if !ok {
				logger.Warn("%s is not a struct", member.Name)
				continue
			}
			expectedSize := uint32(unsafe.Sizeof(l3EndpointT{}))
			if typ.Size != expectedSize {
				logger.Warn("%s has a wrong size, expected %d, got %d", member.Name,
					expectedSize, typ.Size)
				continue
			}
			e := endpointDef{name: member.Name, start: member.Offset.Bytes(), typ: L3}
			endpointDefs = append(endpointDefs, e)
		case types.L4EndpointTypeName:
			typ, ok := member.Type.(*btf.Struct)
			if !ok {
				logger.Warn("%s is not a struct", member.Name)
				continue
			}
			expectedSize := uint32(unsafe.Sizeof(l4EndpointT{}))
			if typ.Size != expectedSize {
				logger.Warn("%s has a wrong size, expected %d, got %d", member.Name,
					expectedSize, typ.Size)
				continue
			}
			e := endpointDef{name: member.Name, start: member.Offset.Bytes(), typ: L4}
			endpointDefs = append(endpointDefs, e)
		case types.TimestampTypeName:
			if err := verifyGadgetUint64Typedef(member.Type); err != nil {
				logger.Warn("%s is not a uint64: %s", member.Name, err)
				continue
			}
			timestampsOffsets = append(timestampsOffsets, member.Offset.Bytes())
		}

		btfSpec, err := btf.LoadKernelSpec()
		if err != nil {
			logger.Warnf("Kernel BTF information not available. Enums won't be resolved to strings")
		}

		if enum, ok := member.Type.(*btf.Enum); ok {
			if btfSpec != nil {
				kernelEnum := &btf.Enum{}
				if err = btfSpec.TypeByName(enum.Name, &kernelEnum); err == nil {
					// Use kernel enum if found
					enum = kernelEnum
				}
			}

			var getter func(data []byte) uint64
			typ := simpleTypeFromBTF(member.Type)
			if typ == nil {
				logger.Warnf("Failed to get type for %s", member.Name)
				continue
			}
			offset := member.Offset.Bytes()
			switch typ.Kind {
			case types.KindUint8:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[uint8](data, offset))
				}
			case types.KindUint16:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[uint16](data, offset))
				}
			case types.KindUint32:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[uint32](data, offset))
				}
			case types.KindUint64:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[uint64](data, offset))
				}
			case types.KindInt8:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[int8](data, offset))
				}
			case types.KindInt16:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[int16](data, offset))
				}
			case types.KindInt32:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[int32](data, offset))
				}
			case types.KindInt64:
				getter = func(data []byte) uint64 {
					return uint64(getAsInteger[int64](data, offset))
				}
			}

			fieldSetter := types.GetSetter[string](t.eventFactory, member.Name)
			enumSetter := func(ev *types.Event, data []byte) {
				val := getter(data)

				for _, v := range enum.Values {
					if val == v.Value {
						fieldSetter(ev, v.Name)
						return
					}
				}

				fieldSetter(ev, "UNKNOWN")
			}
			enumSetters = append(enumSetters, enumSetter)
		}
	}

	return func(data []byte) *types.Event {
		// get mntNsId for enriching the event
		mntNsId := uint64(0)
		if mountNsIdFound {
			mntNsId = getAsInteger[uint64](data, mntNsIdstart)
		}

		// enrich endpoints
		l3endpoints := []types.L3Endpoint{}
		l4endpoints := []types.L4Endpoint{}

		for _, endpoint := range endpointDefs {
			endpointC := (*l3EndpointT)(unsafe.Pointer(&data[endpoint.start]))
			var size int
			switch endpointC.version {
			case 4:
				size = 4
			case 6:
				size = 16
			default:
				logger.Warnf("bad IP version received: %d", endpointC.version)
				continue
			}

			ipBytes := make(net.IP, size)
			copy(ipBytes, endpointC.addr[:])

			l3endpoint := eventtypes.L3Endpoint{
				Addr:    ipBytes.String(),
				Version: endpointC.version,
			}

			switch endpoint.typ {
			case L3:
				endpoint := types.L3Endpoint{
					Name:       endpoint.name,
					L3Endpoint: l3endpoint,
				}
				l3endpoints = append(l3endpoints, endpoint)
			case L4:
				l4EndpointC := (*l4EndpointT)(unsafe.Pointer(&data[endpoint.start]))
				endpoint := types.L4Endpoint{
					Name: endpoint.name,
					L4Endpoint: eventtypes.L4Endpoint{
						L3Endpoint: l3endpoint,
						Port:       l4EndpointC.port,
						Proto:      l4EndpointC.proto,
					},
				}
				l4endpoints = append(l4endpoints, endpoint)
			}
		}

		// handle timestamps
		timestamps := []eventtypes.Time{}
		for _, offset := range timestampsOffsets {
			timestamp := *(*uint64)(unsafe.Pointer(&data[offset]))
			t := gadgets.WallTimeFromBootTime(timestamp)
			timestamps = append(timestamps, t)
		}

		ev := t.eventFactory.NewEvent()

		ev.Type = eventtypes.NORMAL
		ev.MountNsID = mntNsId
		ev.L3Endpoints = l3endpoints
		ev.L4Endpoints = l4endpoints
		ev.Timestamps = timestamps

		// handle enums
		for _, setter := range enumSetters {
			setter(ev, data)
		}

		// set ebpf data
		ev.Blob[types.IndexEBPF] = data

		return ev
	}
}

func (t *Tracer) runTracers(gadgetCtx gadgets.GadgetContext) {
	cb := t.processEventFunc(gadgetCtx)

	for {
		var rawSample []byte

		if t.ringbufReader != nil {
			record, err := t.ringbufReader.Read()
			if err != nil {
				if errors.Is(err, ringbuf.ErrClosed) {
					// nothing to do, we're done
					return
				}
				gadgetCtx.Logger().Errorf("read ring buffer: %w", err)
				return
			}
			rawSample = record.RawSample
		} else if t.perfReader != nil {
			record, err := t.perfReader.Read()
			if err != nil {
				if errors.Is(err, perf.ErrClosed) {
					return
				}
				gadgetCtx.Logger().Errorf("read perf ring buffer: %w", err)
				return
			}

			if record.LostSamples != 0 {
				gadgetCtx.Logger().Warnf("lost %d samples", record.LostSamples)
				continue
			}
			rawSample = record.RawSample
		}

		ev := cb(rawSample)
		t.eventCallback(ev)
	}
}

func (t *Tracer) setEBPFParameters(ebpfParams map[string]types.EBPFParam, gadgetParams *params.Params) {
	t.config.Consts = make(map[string]interface{})
	for varName, paramDef := range ebpfParams {
		p := gadgetParams.Get(paramDef.Key)
		if !p.IsSet() {
			continue
		}
		t.config.Consts[varName] = p.AsAny()
	}
}

func (t *Tracer) runIterInAllNetNs(it *link.Iter, cb func([]byte) *types.Event) ([]*types.Event, error) {
	events := []*types.Event{}
	s := int(t.eventType.Size)

	namespacesToVisit := map[uint64]*containercollection.Container{}
	for _, c := range t.containers {
		namespacesToVisit[c.Netns] = c
	}

	for _, container := range namespacesToVisit {
		err := netnsenter.NetnsEnter(int(container.Pid), func() error {
			reader, err := it.Open()
			if err != nil {
				return err
			}
			defer reader.Close()

			buf, err := io.ReadAll(reader)
			if err != nil {
				return err
			}

			eventsLocal := splitAndConvert(buf, s, cb)
			for _, ev := range eventsLocal {
				// TODO: set all the values here to avoid depending on the enricher?
				ev.NetNsID = container.Netns
			}

			events = append(events, eventsLocal...)

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return events, nil
}

func splitAndConvert(data []byte, size int, cb func([]byte) *types.Event) []*types.Event {
	events := make([]*types.Event, len(data)/size)
	for i := 0; i < len(data)/size; i++ {
		ev := cb(data[i*size : (i+1)*size])
		events[i] = ev
	}
	return events
}

func (t *Tracer) runSnapshotter(gadgetCtx gadgets.GadgetContext) error {
	cb := t.processEventFunc(gadgetCtx)

	events := []*types.Event{}

	for _, l := range t.linksSnapshotters {
		switch l.typ {
		// Iterators that have to be run in the root pid namespace
		case "task":
			buf, err := bpfiterns.Read(l.link)
			if err != nil {
				return fmt.Errorf("reading iterator: %w", err)
			}
			eventsL := splitAndConvert(buf, int(t.eventType.Size), cb)
			events = append(events, eventsL...)
		// Iterators that have to be run on each network namespace
		case "tcp", "udp":
			var err error
			eventsL, err := t.runIterInAllNetNs(l.link, cb)
			if err != nil {
				return fmt.Errorf("reading iterator: %w", err)
			}
			events = append(events, eventsL...)
		}
	}

	t.eventArrayCallback(events)

	return nil
}

func (t *Tracer) Run(gadgetCtx gadgets.GadgetContext) error {
	if err := t.installTracer(gadgetCtx); err != nil {
		t.Close()
		return fmt.Errorf("install tracer: %w", err)
	}

	if t.perfReader != nil || t.ringbufReader != nil {
		go t.runTracers(gadgetCtx)
	}
	if len(t.linksSnapshotters) > 0 {
		return t.runSnapshotter(gadgetCtx)
	}
	gadgetcontext.WaitForTimeoutOrDone(gadgetCtx)

	return nil
}

func (t *Tracer) AttachContainer(container *containercollection.Container) error {
	t.mu.Lock()
	t.containers[container.Runtime.ContainerID] = container
	t.mu.Unlock()

	for _, networkTracer := range t.networkTracers {
		if err := networkTracer.Attach(container.Pid); err != nil {
			return err
		}
	}

	return nil
}

func (t *Tracer) DetachContainer(container *containercollection.Container) error {
	t.mu.Lock()
	delete(t.containers, container.Runtime.ContainerID)
	t.mu.Unlock()

	for _, networkTracer := range t.networkTracers {
		if err := networkTracer.Detach(container.Pid); err != nil {
			return err
		}
	}

	return nil
}

func (t *Tracer) SetMountNsMap(mountnsMap *ebpf.Map) {
	t.config.MountnsMap = mountnsMap
}

func (t *Tracer) SetEventHandler(handler any) {
	nh, ok := handler.(func(ev *types.Event))
	if !ok {
		panic("event handler invalid")
	}
	t.eventCallback = nh
}

func (t *Tracer) SetEventHandlerArray(handler any) {
	nh, ok := handler.(func(ev []*types.Event))
	if !ok {
		panic("event handler invalid")
	}
	t.eventArrayCallback = nh
}
