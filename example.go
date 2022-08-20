package main

import (
	"concurrent/multiflight"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"
)

// MultiGetDataFromDb : 批量从数据库中获取数据
func MultiGetDataFromDb(keys []string)(map[string]interface{}, error) {
	defer func() {
		log.Println("db multi query end")
	}()

	log.Println("db multi query begin")
	result := make(map[string]interface{})
	for _, key := range keys {
		result[key] = "data_mock_" + key
	}
	// 假如接口50ms返回
	time.Sleep(50 * time.Millisecond)

	return result, nil
}

func main() {
	multiFlight := multiflight.NewGroup(50, 5, MultiGetDataFromDb)

	getData := func(requestID int, key string)(string, error) {
		log.Printf("request %v start request ...", requestID)

		// 合并请求
		value, _, _, _ := multiFlight.Do(key)
		//ch := multiFlight.DoChan(key)
		//result := <- ch

		return value.(string), nil
	}

	var wg sync.WaitGroup
	keyPrefix := "order_id_"
	wg.Add(10000)
	for i := 0; i < 10000; i++ {
		// 每生成1000个key休息1s, 观察
		if i % 1000 == 0 {
			time.Sleep(1 * time.Second)
		}
		go func(wg *sync.WaitGroup, requestID int) {
			defer wg.Done()

			// 生成8种个不同的key (每组50个或过了5ms超时时间自动组提交)
			orderIndex := strconv.Itoa(requestID & 7)

			// 生成128种不同的key
			//orderIndex := strconv.Itoa(requestID & 127)
			key := keyPrefix + orderIndex
			value, _ := getData(requestID, key)
			log.Printf("request %v, params key %v, get value: %v", requestID, key, value)
		}(&wg, i)
	}
	wg.Wait()

	fmt.Println("main end")
}
