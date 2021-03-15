// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/api/global"
	"go.opentelemetry.io/otel/api/metric"
	"go.opentelemetry.io/otel/label"
)

type handler func(payload []*Packet)
type recorder func(id, vertexType, operation string, payload []*Packet)

type vertex struct {
	id         string
	vertexType string
	input      *edge
	handler
	connector func(ctx context.Context, b *builder) error
	option    *Option
}

func (v *vertex) cascade(ctx context.Context, b *builder, input *edge) error {
	if v.input != nil && v.vertexType != "stream" {
		input.sendTo(ctx, v.input)
		return nil
	}

	v.option = b.option.merge(v.option)
	v.input = input

	v.record(b.recorder)
	v.metrics(ctx)
	v.span(ctx)
	v.deepCopy()
	v.debug()
	v.recover()
	v.run(ctx)

	b.vertacies[v.id] = v

	return v.connector(ctx, b)
}

func (v *vertex) span(ctx context.Context) {
	h := v.handler

	tracer := global.Tracer(v.vertexType + "." + v.id)

	v.handler = func(payload []*Packet) {
		if *v.option.Span {
			now := time.Now()

			for _, packet := range payload {
				if packet.span == nil {
					packet.newSpan(ctx, tracer, "stream.inject", v.id, v.vertexType)
				}

				packet.span.AddEvent(ctx, "vertex",
					label.String("vertex_id", v.id),
					label.String("vertex_type", v.vertexType),
					label.String("packet_id", packet.ID),
					label.Int64("when", now.UnixNano()),
				)
			}
		}

		h(payload)

		if *v.option.Span {
			now := time.Now()

			for _, packet := range payload {
				if packet.Error != nil {
					packet.span.AddEvent(ctx, "error",
						label.String("vertex_id", v.id),
						label.String("vertex_type", v.vertexType),
						label.String("packet_id", packet.ID),
						label.Int64("when", now.UnixNano()),
						label.Bool("error", packet.Error != nil),
					)
				}
			}
		}

		if v.vertexType == "transmit" {
			for _, packet := range payload {
				if packet.span != nil {
					packet.span.End()
				}
			}
		}
	}
}

func (v *vertex) metrics(ctx context.Context) {
	if *v.option.Metrics {
		h := v.handler

		meter := global.Meter(v.id)

		labels := []label.KeyValue{
			label.String("vertex_id", v.id),
			label.String("vertex_type", v.vertexType),
		}

		inTotalCounter := metric.Must(meter).NewFloat64Counter(v.vertexType + "." + v.id + ".total.incoming")
		outTotalCounter := metric.Must(meter).NewFloat64Counter(v.vertexType + "." + v.id + ".total.outgoing")
		errorsTotalCounter := metric.Must(meter).NewFloat64Counter(v.vertexType + "." + v.id + ".total.errors")
		inCounter := metric.Must(meter).NewInt64ValueRecorder(v.vertexType + "." + v.id + ".incoming")
		outCounter := metric.Must(meter).NewInt64ValueRecorder(v.vertexType + "." + v.id + ".outgoing")
		errorsCounter := metric.Must(meter).NewInt64ValueRecorder(v.vertexType + "." + v.id + ".errors")
		batchDuration := metric.Must(meter).NewInt64ValueRecorder(v.vertexType + "." + v.id + ".duration")

		v.handler = func(payload []*Packet) {
			inCounter.Record(ctx, int64(len(payload)), labels...)
			inTotalCounter.Add(ctx, float64(len(payload)), labels...)
			start := time.Now()
			h(payload)
			duration := time.Since(start)
			failures := 0
			for _, packet := range payload {
				if packet.Error != nil {
					failures++
				}
			}
			outCounter.Record(ctx, int64(len(payload)), labels...)
			outTotalCounter.Add(ctx, float64(len(payload)), labels...)
			errorsCounter.Record(ctx, int64(failures), labels...)
			errorsTotalCounter.Add(ctx, float64(failures), labels...)
			batchDuration.Record(ctx, int64(duration), labels...)
		}
	}
}

func (v *vertex) debug() {
	if *v.option.Debug {
		h := v.handler

		v.handler = func(payload []*Packet) {
			start := time.Now()

			h(payload)

			end := time.Now()

			out := []*Packet{}
			buf := &bytes.Buffer{}
			enc, dec := gob.NewEncoder(buf), gob.NewDecoder(buf)

			_ = enc.Encode(payload)
			_ = dec.Decode(&out)

			for i, value := range out {
				if payload[i].Snapshots == nil {
					payload[i].Snapshots = []*DebugInfo{}
				}
				payload[i].Snapshots = append(payload[i].Snapshots, &DebugInfo{
					ID:       v.id,
					Start:    start,
					End:      end,
					Snapshot: value.Data,
				})
			}
		}
	}
}

func (v *vertex) recover() {
	h := v.handler

	v.handler = func(payload []*Packet) {
		defer func() {
			if r := recover(); r != nil {
				var err error
				var ok bool

				if err, ok = r.(error); !ok {
					err = fmt.Errorf("%v", r)
				}

				ids := make([]string, len(payload))

				for i, packet := range payload {
					ids[i] = packet.ID
				}

				defaultLogger.Error(fmt.Sprintf("panic-recovery [id: %s type: %s error: %v packets: %v]", v.id, v.vertexType, err, ids))
			}
		}()

		h(payload)
	}
}

func (v *vertex) deepCopy() {
	if *v.option.DeepCopy {
		h := v.handler

		v.handler = func(payload []*Packet) {
			out := []*Packet{}
			buf := &bytes.Buffer{}
			enc, dec := gob.NewEncoder(buf), gob.NewDecoder(buf)

			_ = enc.Encode(payload)
			_ = dec.Decode(&out)

			for i, val := range payload {
				out[i].span = val.span
			}

			h(out)
		}
	}
}

func (v *vertex) record(r recorder) {
	if r != nil {
		h := v.handler

		v.handler = func(payload []*Packet) {
			r(v.id, v.vertexType, "start", payload)
			h(payload)
		}
	}
}

func (v *vertex) run(ctx context.Context) {
	go func() {
	Loop:
		for {
			select {
			case <-ctx.Done():
				break Loop
			case data := <-v.input.channel:
				if len(data) < 1 {
					continue
				}

				if *v.option.FIFO {
					v.handler(data)
				} else {
					go v.handler(data)
				}
			}
		}
	}()
}
