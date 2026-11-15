// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otap

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/collector/consumer"
	"google.golang.org/grpc"
)

const (
	// DefaultMaxRecvMessageBytes is the default maximum size of a single received
	// OTAP message. Batches are considerably larger than OTLP payloads with the
	// same content, because they carry several Arrow record batches.
	DefaultMaxRecvMessageBytes = 16 << 20

	// DefaultMaxConcurrentStreams bounds the number of concurrent streams a single
	// client connection may open. Together with the per-stream Arrow memory limit
	// it bounds the decoding memory a single client can pin.
	DefaultMaxConcurrentStreams = 32
)

// ServerOptions configures the gRPC server serving OTAP.
type ServerOptions struct {
	// ReceiverOptions configures the OTAP receiver itself.
	ReceiverOptions
	// MaxRecvMessageBytes is the maximum size of a received message in bytes.
	// Zero selects [DefaultMaxRecvMessageBytes].
	MaxRecvMessageBytes int
	// MaxConcurrentStreams is the maximum number of concurrent streams per client
	// connection. Zero selects [DefaultMaxConcurrentStreams].
	MaxConcurrentStreams uint32
}

// NewGRPCServer returns a gRPC server serving OTAP metrics decoded through the
// given ingestion consumer. Only the OTAP metrics service is registered.
//
// The returned server is meant to be served on a dedicated listener by the
// caller; it is not multiplexed onto the Prometheus web handler.
func NewGRPCServer(logger *slog.Logger, reg prometheus.Registerer, c consumer.Metrics, opts ServerOptions) *grpc.Server {
	if opts.MaxRecvMessageBytes <= 0 {
		opts.MaxRecvMessageBytes = DefaultMaxRecvMessageBytes
	}
	if opts.MaxConcurrentStreams == 0 {
		opts.MaxConcurrentStreams = DefaultMaxConcurrentStreams
	}

	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(opts.MaxRecvMessageBytes),
		grpc.MaxConcurrentStreams(opts.MaxConcurrentStreams),
	)
	NewReceiver(logger, reg, c, opts.ReceiverOptions).RegisterServer(srv)
	return srv
}
