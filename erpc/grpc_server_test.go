package erpc

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func TestGrpcClientIP_IgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{
			"127.0.0.1": {},
		},
		trustedIPHeaders: []string{"x-forwarded-for"},
	}
	md := metadata.New(map[string]string{
		"x-forwarded-for": "127.0.0.1",
	})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.25"), Port: 9000},
	})

	assert.Equal(t, "198.51.100.25", gs.grpcClientIP(ctx, md))
}

func TestGrpcClientIP_UsesConfiguredForwardedHeaderFromTrustedPeer(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{
			"127.0.0.1": {},
		},
		trustedIPHeaders: []string{"x-forwarded-for"},
	}
	md := metadata.New(map[string]string{
		"x-forwarded-for": "203.0.113.9",
	})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000},
	})

	assert.Equal(t, "203.0.113.9", gs.grpcClientIP(ctx, md))
}

func TestGrpcClientIP_IgnoresForwardedHeaderWhenHeaderNotTrusted(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{
			"127.0.0.1": {},
		},
	}
	md := metadata.New(map[string]string{
		"x-forwarded-for": "203.0.113.9",
	})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000},
	})

	assert.Equal(t, "127.0.0.1", gs.grpcClientIP(ctx, md))
}
