## 1. singleflight 并发模式的思考

`singleflight`是Go开发组提供的一个拓展并发原语。它可以使多个goroutine同时调用一个函数时，只让第一个goroutine真实的调用该函数，拿到结果后，将数据拷贝分发给其他goroutine，这样可以减少并发调用量。

在面对秒杀等大并发读请求场景时，`singleflight`能发挥出巨大威力，它可以将N次对相同资源的请求降到1次。对于秒杀服务，我们设计缓存时要解决缓存穿透、缓存击穿和缓存雪崩问题，用`singleflight`来解决缓存击穿问题再合适不过了，只要对同一个key的并发请求合并成1次数据库查询就好了，因为是缓存查询，不用考虑幂等性问题。

![singleflight提交设计.png](https://p1-juejin.byteimg.com/tos-cn-i-k3u1fbpfcp/9842c93585ee4466a100360bf9c5d264~tplv-k3u1fbpfcp-watermark.image?)

事实上，Go生态圈知名的缓存框架`groupcache`和其他缓存库中都使用了`singleflight`来防止缓存击穿。

真实的业务场景中，热key往往不止一个；也或许没有热key，但是不同key的请求量总和很大，当很多key同时过期时，穿透到数据库的请求让系统不堪重负，面对这样的“缓存雪崩”场景，`singleflight`就解决不了了。我在想既然mysql提供了in条件查询，redis也提供了pipeline的能力，我们能不能把这些对不同数据资源的单次查询请求聚合成一次批量查询请求，拿到查询结果后再按需分发呢？如下图所示：

![multiFlight组提交设计.png](https://p6-juejin.byteimg.com/tos-cn-i-k3u1fbpfcp/ee4a97be3cfc4e1e87813aad7527bda5~tplv-k3u1fbpfcp-watermark.image?)

这就是我设计`multiflight`的基本思路: 在`singleflight`的基础上，将对不同资源的一组请求聚合成一次批量请求，拿到结果后再按需分发给各个资源请求方，达到减少并发调用量的目的。

项目代码传送门 [`multiflight`源码实现](https://github.com/GuoZhaoran/multiflight)

## 2. multiflight 使用示例详解

`multiflight`包使用起来和`singleflight`一样简单，如果你对`singleflight`的使用还不太清楚，可以自行查找资料学习，这并不影响`multiflight`的学习使用，这里就不再对`singleflight`的使用做介绍了。
回到正题，我们拿不同订单详情并发请求服务举例，有50个客户端并发请求order_id_{0-49}的订单详情，从数据库中获取到的订单详情分别对应mock数据data_mock_order_id_{0-49}。示例代码如下：

```
package main

import (
   "concurrent/multiflight"
   "fmt"
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
```

下面我分三步来介绍`multifight`的使用过程。

### 2.1 实例化任务组管理器

实例化任务组管理器的代码片段是：

```
// 创建一个组容量16, 任务提交周期为5ms的multiFlight
multiFlight := multiflight.NewGroup(16, 5, MultiGetDataFromDb)
```

正如前边所说，`multiflight`的核心实现是把```order_id_1、order_id_2 ...... order_id_N```详情的请求合并成一个数据库的in查询操作，所以需要将这些单个订单详情的请求给划分到一个任务组中去，实现这个功能的组件就是“任务组管理器”，它有三个基本的属性需要在实例化的时候进行设置：

- 容量(Cap)，也就是组管理器的每个组最多包含多少种任务，我们可以根据业务需要进行灵活设置。

- 组等待时间(Period)，单位是ms，因为请求流量是不可控的，我们不能一直阻塞等待组中的任务种类达到容量后再批量请求，组等待时间是指程序最多等待多久可以自动提交任务。

- 组提交函数(CommitFunc)，组提交函数需要根据业务由开发者定义，它需要满足函数签名```func([]string)(map[string]interface{}, error)```。其中的参数是我们注册到组中的请求资源参数，也就是：```order_id_1、order_id_2 ...... order_id_N```，开发者拿到这些参数后需要根据业务去聚合查询得到数据，查询数据源不同，处理方式也不一样。如果查询的是redis，就需要拼接 `pipeline` 的请求指令；如果是查询的mysql，则需要拼接成类似` select * from table where order_id in (order_id_1, order_id_2, order_id_N)` 的条件查询语句。对于返回结果，组提交函数也定义了结构`map[string]interface{}`，开发者需要将每个查询key对应的结果给映射起来，这样程序才能分发相应数据给资源的请求者。我们例子中使用mock数据的方式实现了CommitFunc :

```
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
```

下边是任务组管理器的示意图：

![组任务管理器设计.png](https://p1-juejin.byteimg.com/tos-cn-i-k3u1fbpfcp/206a8c9f864e4e0a8a4dc6f480f98eca~tplv-k3u1fbpfcp-watermark.image?)

### 2.2 注册任务到任务组管理器

这一步很好理解，就是有新的客户端请求任务了，就将任务注册到任务组管理器中，等待任务组管理器中的任务处理完成后，接收分发到的请求结果。和`singleflight`一样，`multiflight`也提供了同步接收返回结果的方法`Do`和返回一个channel由客户端异步处理的方法`DoChan`两种方式。我们这里演示的是同步处理的方式，代码片段如下：

```
......
getData := func(requestID int, key string)(string, error) {
   // 注册请求，实时等待响应
   value, _, _, _ := multiFlight.Do(key)
   // 也可以使用DoChan返回一个chan, 异步的处理请求
   //ch := multiFlight.DoChan(key)
   //result := <- ch

   return value.(string), nil
}
......
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
```

这一步有两个容易理解错的点，第一个是任务组管理器的容量指的是一个任务组可以存放不同任务种类的最大个数，比如一个客户端已经将`order_id_1`的请求任务添加到了一个任务组中，之后又有一个客户端也将`order_id_1`的请求任务添加到这个任务组，那么这个任务组中任务种类是没有发生变化的；后边的客户端的请求任务也只是加到了`order_id_1`的`singleflight`中去，`multiflight`本身就包含了`singleflight`的功能，下图能很好的帮助读者理解这一点：

![向组任务管理器中添加任务.png](https://p3-juejin.byteimg.com/tos-cn-i-k3u1fbpfcp/6387e8fc6ec64d3d8d760eefebcd2fe8~tplv-k3u1fbpfcp-watermark.image?)

第二个容易理解错的点是：任务组管理器同时管理着多组任务，每一组任务都有一个组编号(是一个uint32类型的整数，组任务管理器每产生一组任务，就会给其分配一个组编号)。比如任务组1的任务种类已经满了或者到了组等待时间，程序就会触发调用组提交函数`CommitFunc`去聚合请求并获取数据，这个过程相对耗时(涉及到网络IO操作)，例如这个过程耗时10ms，在这10ms期间客户端可以继续向组1中提交相同种类的任务，如果有组1中不存在的客户端提交的任务种类，那么会自动提交到组2当中去；如果组2也达到了任务容量，也是相同的原理，客户端提交的任务种类在组1、组2中存在，那么可以自动提交到相应的组中，否则提交到组3中，至于组2、组3是如何创建的以及组1、组2、组3是什么时候被销毁的，这些都是`multiflight`内部实现的，感兴趣的读者可以阅读源码学习(仅一百多行代码实现了这么复杂的功能)。下图是帮助读者理解`multiflight`管理多组任务的一个示意图：

![组任务管理器是如何管理多组任务的.png](https://p6-juejin.byteimg.com/tos-cn-i-k3u1fbpfcp/cd1ed3b20b914b40bf40050023b3846b~tplv-k3u1fbpfcp-watermark.image?)

### 2.3 返回结果的处理

最后要做的就是对`multiflight`分配的返回结果进行处理了，正确的处理各种服务异常情况是非常重要的。下边给出一些正确使用`multiflight`以及面对异常响应结果的处理建议。

先来看一下`multiflight`处理请求后返回的结果吧：

```
type Result struct {
   Val    interface{}     //返回的具体信息
   Err    error  //错误信息
   Shared bool  //是否被共享(singleflight中有多个相同任务)
   Empty bool  //是否没有响应相关的信息
}
```

`multiflight`的任务返回结果一共有四个值，比`singleflight`处理多返回了一个值empty，标识是否查询到请求的响应信息。比如`multiflight`组提交中包含了```order_id_1、order_id_2、order_id_3```的请求任务，但是从数据库中只查询到了```order_id_1、order_id_2```的详情，那么```order_id_3```的请求方就会收到emtpy为true的响应信息，至于后续该如何处理就交给开发者决定了。

第一个建议是给组提交函数`CommitFunc`添加超时机制，因为`CommitFunc`上阻塞等待着很多服务请求，如果下游服务迟迟不返回结果，或者开发者写的代码有bug，就会使得一大批客户端得不到请求。通常业务中使用```context```和```select```配合实现超时控制，例如：

```
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
```

第二个建议是当`multiflight`请求返回错误信息时，要制定合理的`熔断`或`降级`策略。首先`multiflight`并不会自动重试，组提交函数只会执行一次，超时失败了或者下游服务异常了，都会返回相应的错误信息。对于`multiflight`为什么不自动重试，我是这么理解的：重试这个特性应该是和业务强相关的，只有部分查询接口和支持幂等的写接口才适合重试，盲目重试可以把自己干死，拒绝是一门学问，也需要知识底蕴。对于异常处理策略需要开发者自己根据业务自定义，比如`multiflight`处理失败了，可以使用`singleflight`再次降级请求，`singleflight`如果也失败了，就直接返回给用户友好的提示信息；当服务中有大量错误时，可以采用熔断策略，不再使用`multiflight`和`singleflight`，这些策略都是对服务的一种保护。

第三个建议是重新评估`限流`策略，`multiflight`和`singleflight`对`限流`的值是有影响的，一般来讲使用这两种并发原语能够明显降低数据库的请求量，提升服务的处理能力，也就是说影响是正相关的，但这也并不是绝对的。需要根据真实场景下的压测结果进行评估。

最后我们根据分析下例子中的返回结果吧：

```
2022/08/20 19:02:58 db multi query begin, task key num:12
2022/08/20 19:02:58 db multi query begin, task key num:2
2022/08/20 19:02:58 db multi query begin, task key num:16
2022/08/20 19:02:58 db multi query begin, task key num:16
2022/08/20 19:02:58 db multi query end
2022/08/20 19:02:58 request 11, params key order_id_11, get value: data_mock_order_id_11
2022/08/20 19:02:58 request 0, params key order_id_0, get value: data_mock_order_id_0
2022/08/20 19:02:58 request 10, params key order_id_10, get value: data_mock_order_id_10
2022/08/20 19:02:58 request 3, params key order_id_3, get value: data_mock_order_id_3
2022/08/20 19:02:58 request 4, params key order_id_4, get value: data_mock_order_id_4
2022/08/20 19:02:58 request 6, params key order_id_6, get value: data_mock_order_id_6
2022/08/20 19:02:58 request 8, params key order_id_8, get value: data_mock_order_id_8
2022/08/20 19:02:58 request 7, params key order_id_7, get value: data_mock_order_id_7
2022/08/20 19:02:58 request 5, params key order_id_5, get value: data_mock_order_id_5
2022/08/20 19:02:58 request 9, params key order_id_9, get value: data_mock_order_id_9
2022/08/20 19:02:58 request 1, params key order_id_1, get value: data_mock_order_id_1
2022/08/20 19:02:58 request 2, params key order_id_2, get value: data_mock_order_id_2
2022/08/20 19:02:58 db multi query end
2022/08/20 19:02:58 db multi query end
2022/08/20 19:02:58 db multi query begin, task key num:4
2022/08/20 19:02:58 request 12, params key order_id_12, get value: data_mock_order_id_12
2022/08/20 19:02:58 db multi query end
2022/08/20 19:02:58 request 20, params key order_id_20, get value: data_mock_order_id_20
2022/08/20 19:02:58 request 26, params key order_id_26, get value: data_mock_order_id_26
2022/08/20 19:02:58 request 14, params key order_id_14, get value: data_mock_order_id_14
2022/08/20 19:02:58 request 15, params key order_id_15, get value: data_mock_order_id_15
2022/08/20 19:02:58 request 24, params key order_id_24, get value: data_mock_order_id_24
2022/08/20 19:02:58 request 36, params key order_id_36, get value: data_mock_order_id_36
2022/08/20 19:02:58 request 22, params key order_id_22, get value: data_mock_order_id_22
2022/08/20 19:02:58 request 21, params key order_id_21, get value: data_mock_order_id_21
2022/08/20 19:02:58 request 42, params key order_id_42, get value: data_mock_order_id_42
2022/08/20 19:02:58 request 41, params key order_id_41, get value: data_mock_order_id_41
2022/08/20 19:02:58 request 29, params key order_id_29, get value: data_mock_order_id_29
2022/08/20 19:02:58 request 17, params key order_id_17, get value: data_mock_order_id_17
2022/08/20 19:02:58 request 13, params key order_id_13, get value: data_mock_order_id_13
2022/08/20 19:02:58 request 25, params key order_id_25, get value: data_mock_order_id_25
2022/08/20 19:02:58 request 16, params key order_id_16, get value: data_mock_order_id_16
2022/08/20 19:02:58 request 19, params key order_id_19, get value: data_mock_order_id_19
2022/08/20 19:02:58 request 44, params key order_id_44, get value: data_mock_order_id_44
2022/08/20 19:02:58 request 43, params key order_id_43, get value: data_mock_order_id_43
2022/08/20 19:02:58 request 28, params key order_id_28, get value: data_mock_order_id_28
2022/08/20 19:02:58 request 32, params key order_id_32, get value: data_mock_order_id_32
2022/08/20 19:02:58 request 23, params key order_id_23, get value: data_mock_order_id_23
2022/08/20 19:02:58 request 30, params key order_id_30, get value: data_mock_order_id_30
2022/08/20 19:02:58 request 18, params key order_id_18, get value: data_mock_order_id_18
2022/08/20 19:02:58 request 31, params key order_id_31, get value: data_mock_order_id_31
2022/08/20 19:02:58 request 38, params key order_id_38, get value: data_mock_order_id_38
2022/08/20 19:02:58 request 34, params key order_id_34, get value: data_mock_order_id_34
2022/08/20 19:02:58 request 35, params key order_id_35, get value: data_mock_order_id_35
2022/08/20 19:02:58 request 40, params key order_id_40, get value: data_mock_order_id_40
2022/08/20 19:02:58 request 33, params key order_id_33, get value: data_mock_order_id_33
2022/08/20 19:02:58 request 37, params key order_id_37, get value: data_mock_order_id_37
2022/08/20 19:02:58 request 49, params key order_id_49, get value: data_mock_order_id_49
2022/08/20 19:02:58 request 39, params key order_id_39, get value: data_mock_order_id_39
2022/08/20 19:02:58 request 27, params key order_id_27, get value: data_mock_order_id_27
2022/08/20 19:02:58 db multi query end
2022/08/20 19:02:58 request 45, params key order_id_45, get value: data_mock_order_id_45
2022/08/20 19:02:58 request 48, params key order_id_48, get value: data_mock_order_id_48
2022/08/20 19:02:58 request 46, params key order_id_46, get value: data_mock_order_id_46
2022/08/20 19:02:58 request 47, params key order_id_47, get value: data_mock_order_id_47
```

根据返回结果日志可以看出，`multiflight`起了5个任务组分别处理50个客户端的不同请求，每组分别处理了12、2、16、16、4个任务，加起来正好50个。也就是说本来需要50次服务请求和数据库IO才能解决的问题，使用`multiflight`只用了5次数据库IO查询就完成了，因为是不同的资源请求，这是使用`singleflight`所不能完成的。

## 3. 小结

`singleflight`通过合并请求和数据资源共享的思想有效解决了“缓存击穿”的问题，根据我的了解，`singleflight`在很多Go缓存库和一些大公司的业务场景中都得到了应用。第一次阅读`singleflight`的源码时，我也被这100多行代码惊艳到了。不过`singleflight`只是解决了单个key的并发请求问题，我就想从“线”到“面”，结合mysql的批量查询，redis的pipeline等功能，实现一个既能够聚合不同key实现批量查询，又能实现每个相同key请求的资源共享数据的`multiflight`并发原语。

在微服务盛行的时代，服务越拆越细，也就导致了很多资源请求被放大很多倍。我觉得`singleflight`和`multiflight`能够缓解高并发请求下数据库的访问量过大的问题。当然使用它们所带来的麻烦也不少，比如“一个出错、全部出错”；还有分布式链路追踪下，合并请求会给问题排查带来很多困扰；业务代码上，实现一个健壮的高性能服务，可能需要考虑更多异常情况。

权衡利弊之后，笔者还是期望大家多提出一些好的建议，争取能让`multiflight`在适合它的业务场景中用起来，发挥出它应用的价值。欢迎大家添加我的微信号three-thousand-ink进行交流。


















































