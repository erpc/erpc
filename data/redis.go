package data

import (
	"context"
	"github.com/redis/go-redis/v9"
	"log"
)

var ctx = context.Background()

type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(addr, password string, db int) *RedisStore {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	return &RedisStore{client: rdb}
}

func (r *RedisStore) Get(key string) (string, error) {
	return r.client.Get(ctx, key).Result()
}

func (r *RedisStore) Set(key string, value string) error {
	return r.client.Set(ctx, key, value, 0).Err()
}

func (r *RedisStore) Scan(prefix string) ([]string, error) {
	var keys []string
	iter := r.client.Scan(ctx, 0, prefix+"*", 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func (r *RedisStore) Delete(key string) error {
	return r.client.Del(ctx, key).Err()
}

func main() {
	store := NewRedisStore("localhost:6379", "", 0)

	// Example usage
	if err := store.Set("key1", "value1"); err != nil {
		log.Fatal(err)
	}

	value, err := store.Get("key1")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Got value:", value)

	keys, err := store.Scan("key")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Scanned keys:", keys)

	if err := store.Delete("key1"); err != nil {
		log.Fatal(err)
	}
}
