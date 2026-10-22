package data

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

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
	sharedPostgres sharedContainer
	sharedDynamo   sharedContainer
)

func TestMain(m *testing.M) {
	code := m.Run()
	for _, sc := range []*sharedContainer{&sharedPostgres, &sharedDynamo} {
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
