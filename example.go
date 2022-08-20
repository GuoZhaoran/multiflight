package main

import (
	"concurrent/multiflight"
	"context"
	"fmt"
	"golang.org/x/sync/singleflight"
	"log"
	"math/rand"
	"strconv"
	"sync"
	"time"
)

// MultiGetDataFromDb : 批量从数据库中获取数据
func MultiGetDataFromDb(keys []string)(map[string]interface{}, error) {
	defer func() {
		log.Println("db multi query end")
	}()

	// 请求计数
	log.Println("db multi query begin, task key num:" + strconv.Itoa(len(keys)))
	result := make(map[string]interface{})
	for _, key := range keys {
		result[key] = "data_mock_" + key
	}
	// 假如接口10ms返回
	time.Sleep(10 * time.Millisecond)

	return result, nil
}

func RegisterCommitFuncToMultiflight(ctx context.Context, multiflight *multiflight.Group, keys []string) error {
	multiflight.CommitFunc = func(keys []string)(map[string]interface{}, error) {
		result := make(chan map[string]interface{})

		select {
		case r := <-result:
			return r, nil
		case <-ctx.Done():
			return map[string]interface{}{}, ctx.Err()
		}
	}

	return nil
}

func main() {
	// 创建一个组容量16, 任务提交周期为5ms的multiFlight
	multiFlight := multiflight.NewGroup(16, 5, MultiGetDataFromDb)

	var wg sync.WaitGroup
	keyPrefix := "order_id_"
	wg.Add(50)
	for i := 0; i < 50; i++ {
		// 每个请求有30%的机会能够睡眠1ms
		randN := rand.Intn(10)
		if randN < 3 {
			time.Sleep(1 * time.Millisecond)
		}

		go func(wg *sync.WaitGroup, requestID int) {
			defer wg.Done()

			// 生成不同的key
			key := keyPrefix +strconv.Itoa(requestID)
			value, _, _, _ := multiFlight.Do(key)
			log.Printf("request %v, params key %v, get value: %v", requestID, key, value)
		}(&wg, i)
	}
	wg.Wait()

	fmt.Println("main end")
}
