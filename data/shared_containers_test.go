package data

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Shared containers are started lazily, once per test binary, by the first
// test that needs them, and terminated by TestMain. Each test isolates its
// data with its own table name, so sharing a server is safe.
type sharedContainer struct {
	once sync.Once
	c    testcontainers.Container
	addr string
	err  error
}

var (
	sharedPostgres      sharedContainer
	sharedDynamo        sharedContainer
	sharedValkeyCluster sharedContainer
)

func TestMain(m *testing.M) {
	code := m.Run()
	for _, sc := range []*sharedContainer{&sharedPostgres, &sharedDynamo, &sharedValkeyCluster} {
		if sc.c != nil {
			_ = sc.c.Terminate(context.Background())
		}
	}
	os.Exit(code)
}

// sharedPostgresURI returns a connection URI for a Postgres server shared by
// the whole test binary.
func sharedPostgresURI(t *testing.T) string {
	t.Helper()
	sharedPostgres.once.Do(func() {
		ctx := context.Background()
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "postgres:15-alpine",
				Env:          map[string]string{"POSTGRES_PASSWORD": "password"},
				ExposedPorts: []string{"5432/tcp"},
				WaitingFor: wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(2 * time.Minute),
			},
			Started: true,
		})
		sharedPostgres.c = c
		if err != nil {
			sharedPostgres.err = err
			return
		}
		host, err := c.Host(ctx)
		if err != nil {
			sharedPostgres.err = err
			return
		}
		port, err := c.MappedPort(ctx, "5432")
		if err != nil {
			sharedPostgres.err = err
			return
		}
		sharedPostgres.addr = fmt.Sprintf("postgres://postgres:password@%s:%s/postgres?sslmode=disable", host, port.Port())
	})
	if sharedPostgres.err != nil {
		t.Fatalf("failed to start shared postgres container: %v", sharedPostgres.err)
	}
	return sharedPostgres.addr
}

// sharedDynamoEndpoint returns the endpoint of a DynamoDB Local server shared
// by the whole test binary.
func sharedDynamoEndpoint(t *testing.T) string {
	t.Helper()
	sharedDynamo.once.Do(func() {
		ctx := context.Background()
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "amazon/dynamodb-local",
				ExposedPorts: []string{"8000/tcp"},
				WaitingFor:   wait.ForListeningPort("8000/tcp"),
			},
			Started: true,
		})
		sharedDynamo.c = c
		if err != nil {
			sharedDynamo.err = err
			return
		}
		host, err := c.Host(ctx)
		if err != nil {
			sharedDynamo.err = err
			return
		}
		port, err := c.MappedPort(ctx, "8000")
		if err != nil {
			sharedDynamo.err = err
			return
		}
		sharedDynamo.addr = fmt.Sprintf("http://%s:%s", host, port.Port())
	})
	if sharedDynamo.err != nil {
		t.Fatalf("failed to start shared dynamodb container: %v", sharedDynamo.err)
	}
	return sharedDynamo.addr
}

// sharedValkeyClusterAddr returns the seed address of a three-master Valkey
// cluster, with 16 numbered databases, shared by the whole test binary. The nodes announce 127.0.0.1 on
// host ports bound one to one, so the slot map the client discovers is
// reachable from the test process.
func sharedValkeyClusterAddr(t *testing.T) string {
	t.Helper()
	sharedValkeyCluster.once.Do(func() {
		ports := make([]int, 3)
		for i := range ports {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				sharedValkeyCluster.err = err
				return
			}
			ports[i] = l.Addr().(*net.TCPAddr).Port
			_ = l.Close()
		}

		var script, nodes strings.Builder
		exposed := make([]string, 0, len(ports))
		bindings := network.PortMap{}
		for i, p := range ports {
			fmt.Fprintf(&script, "valkey-server --port %d --cluster-enabled yes --cluster-databases 16 --cluster-port %d --cluster-config-file nodes-%d.conf --cluster-announce-ip 127.0.0.1 --dir /tmp --save '' --appendonly no --daemonize yes\n", p, 17000+i, p)
			fmt.Fprintf(&nodes, " 127.0.0.1:%d", p)
			exposed = append(exposed, fmt.Sprintf("%d/tcp", p))
			bindings[network.MustParsePort(fmt.Sprintf("%d/tcp", p))] = []network.PortBinding{
				{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: strconv.Itoa(p)},
			}
		}
		fmt.Fprintf(&script, "until valkey-cli -p %d ping >/dev/null 2>&1; do sleep 0.1; done\n", ports[2])
		fmt.Fprintf(&script, "valkey-cli --cluster create%s --cluster-replicas 0 --cluster-yes\n", nodes.String())
		fmt.Fprintf(&script, "until valkey-cli -p %d cluster info | grep -q cluster_state:ok; do sleep 0.1; done\n", ports[0])
		script.WriteString("echo CLUSTER-READY\nsleep infinity\n")

		ctx := context.Background()
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "valkey/valkey:9.0-alpine",
				Entrypoint:   []string{"sh", "-c", script.String()},
				ExposedPorts: exposed,
				HostConfigModifier: func(hc *container.HostConfig) {
					hc.PortBindings = bindings
				},
				WaitingFor: wait.ForLog("CLUSTER-READY").WithStartupTimeout(2 * time.Minute),
			},
			Started: true,
		})
		sharedValkeyCluster.c = c
		if err != nil {
			sharedValkeyCluster.err = err
			return
		}
		sharedValkeyCluster.addr = fmt.Sprintf("127.0.0.1:%d", ports[0])
	})
	if sharedValkeyCluster.err != nil {
		t.Fatalf("failed to start shared valkey cluster container: %v", sharedValkeyCluster.err)
	}
	return sharedValkeyCluster.addr
}
