package multiflight

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

const (
	defaultPeriod = 5   // 默认5ms后组提交
	defaultGroupCapacity = 32   //默认的任务组容量, 当请求达到组容量时会自动提交
)

// errGoexit indicates the runtime.Goexit was called in
// the user given function.
var errGoexit = errors.New("runtime.Goexit was called")

// A panicError is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type panicError struct {
	value interface{}
	stack []byte
}

// Error implements error interface.
func (p *panicError) Error() string {
	return fmt.Sprintf("%v\n\n%s", p.value, p.stack)
}

func newPanicError(v interface{}) error {
	stack := debug.Stack()

	// The first line of the stack trace is of the form "goroutine N [status]:"
	// but by the time the panic reaches Do the goroutine may no longer exist
	// and its status will have changed. Trim out the misleading line.
	if line := bytes.IndexByte(stack[:], '\n'); line >= 0 {
		stack = stack[line+1:]
	}
	return &panicError{value: v, stack: stack}
}

// call is an in-flight or completed singleflight.Do call
type call struct {
	wg sync.WaitGroup

	// These fields are written once before the WaitGroup is done
	// and are only read after the WaitGroup is done.
	val interface{}
	err error
	empty bool

	// These fields are read and written with the singleflight
	// mutex held before the WaitGroup is done, and are read but
	// not written after the WaitGroup is done.
	dups  int
	chans []chan<- Result
}

// Group represents a class of work and forms a namespace in
// which units of work can be executed with duplicate suppression.
type Group struct {
	mu sync.Mutex       //保护整个结构体
	m  map[string]*call  //懒加载使用
	numberId uint32   //组编号id计数器, 从0开始依次自增计数
	taskPool map[uint32][]string    //每组请求key的池子, 容量为Cap，池子的key是组id, 容量满了以后会立即触发组提交
	Cap uint32    //组容量大小, 达到容量后会自动提交
	Period uint32   //单位毫秒, 默认10ms
	CommitFunc func([]string)(map[string]interface{}, error)    //组提交函数, 使用时需要根据用户自定义, 程序会根据string分发任务给单独的singleFlight
}

// Result holds the results of Do, so they can be passed
// on a channel.
type Result struct {
	Val    interface{}
	Err    error
	Shared bool
	Empty bool
}

// NewGroup : 创建一个新的组，方便后续工作
func NewGroup(cap, period uint32, commitFunc func([]string)(map[string]interface{}, error)) *Group {
	group := &Group{
		m : make(map[string]*call),
		numberId: 1,
		Cap: defaultGroupCapacity,
		Period: defaultPeriod,
		CommitFunc: commitFunc,
	}
	if cap >= 2 {
		group.Cap = cap
	}
	if period > 1 {
		group.Period = period
	}
	group.taskPool = make(map[uint32][]string)
	group.taskPool[group.numberId] = make([]string, 0, cap)

	return group
}

// Do : 向multiplied Flight中注册事件, 注册的事件需要 Wait 返回结果的通知
func (g *Group) Do(key string) (v interface{}, err error, empty, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		c.wg.Wait()

		if e, ok := c.err.(*panicError); ok {
			panic(e)
		} else if c.err == errGoexit {
			runtime.Goexit()
		}
		return c.val, c.err, c.empty, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	// 将当前key添加到组中, 后续检查满足条件可进行组提交
	curNumberId, isFull, isFirst := g.addToPool(key)
	log.Printf("multi request curNumberId: %d, isFull:%v, isFirst:%v", curNumberId, isFull, isFirst)
	g.mu.Unlock()
	// 满了, 直接进行组提交就可以
	if isFull {
		g.commit(curNumberId)
	}
	// 添加计时器异步执行, 若是达到时间阈值则自动提交
	if isFirst {
		time.AfterFunc(time.Duration(g.Period) * time.Millisecond, func() {
			g.commit(curNumberId)
		})
	}
	c.wg.Wait()

	return c.val, c.err, c.empty, c.dups > 0
}

// DoChan is like Do but returns a channel that will receive the
// results when they are ready.
//
// The returned channel will not be closed.
func (g *Group) DoChan(key string) <-chan Result {
	ch := make(chan Result, 1)
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		c.chans = append(c.chans, ch)
		g.mu.Unlock()
		return ch
	}
	c := &call{chans: []chan<- Result{ch}}
	c.wg.Add(1)
	// 将当前key添加到组中, 后续检查满足条件可进行组提交
	curNumberId, isFull, isFirst := g.addToPool(key)
	g.m[key] = c
	g.mu.Unlock()// 满了, 直接进行组提交就可以
	if isFull {
		go g.commit(curNumberId)
	}
	// 添加计时器异步执行, 若是达到时间阈值则自动提交
	if isFirst {
		time.AfterFunc(time.Duration(g.Period) * time.Millisecond, func() {
			g.commit(curNumberId)
		})
	}

	return ch
}

// commit : 组提交, 一组任务提交后，批量等待任务分发和返回结果
func (g *Group) commit(numberId uint32) {
	//取出任务参数
	g.mu.Lock()
	taskParams, exists := g.taskPool[numberId]
	if exists {
		// 取到数据后删除numberId, 防止下个任务重复处理造成错误
		delete(g.taskPool, numberId)
	} else {
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()

	g.doCall(taskParams)
}

// addToPool : 将任务key添加到组当中
// curNumberId : 当前的numberId
// isFull : 任务组是否满了, 如果满了, 就达到了组提交的条件
// isFirst : 是否是组内的第一个任务, 如果是组内的第一个任务, 组提交程序程序就会启动一个timer，等待超时后进行组提交
func (g *Group) addToPool(key string) (curNumberId uint32, isFull bool, isFirst bool) {
	g.taskPool[g.numberId] = append( g.taskPool[g.numberId], key)      // 这里不需要添加，因为确定curTaskPool不会扩容
	if len(g.taskPool[g.numberId]) == 1 {    //如果是组内第一个任务，直接返回即可
		return g.numberId, false, true
	}

	// 如果添加满了, 则初始化一个新的组给接下来的流量使用
	if len(g.taskPool[g.numberId]) == cap(g.taskPool[g.numberId]) {
		oldNumberId := g.numberId
		g.numberId += 1   //自增g.numberId
		g.taskPool[g.numberId] = make([]string, 0, g.Cap)  //这里满量的容量，后续不需要扩容

		return oldNumberId, true, false  //这里需要返回旧的oldNumberId, 因为该组需要组提交，新的组继续积累流量
	}

	return g.numberId, false, false
}

// doCall : 组提交后真实请求资源的逻辑
func (g *Group) doCall(taskParams []string) {
	var err error
	ret := make(map[string]interface{})     //请求资源结果
	normalReturn := false
	recovered := false

	// use double-defer to distinguish panic from runtime.Goexit,
	// more details see https://golang.org/cl/134395
	defer func() {
		// the given function invoked runtime.Goexit
		if !normalReturn && !recovered {
			err = errGoexit
		}
		for _, key := range taskParams {
			g.m[key].wg.Done()
		}

		g.mu.Lock()
		defer g.mu.Unlock()

		// 循环遍历释放请求, 因为大家已经都都拿到了自己的资源
		for _, key := range taskParams {
			c := g.m[key]
			// 将c从数组中移除
			delete(g.m, key)

			if e, ok := err.(*panicError); ok {
				// In order to prevent the waiting channels from being blocked forever,
				// needs to ensure that this panic cannot be recovered.
				if len(c.chans) > 0 {
					go panic(e)
					select {} // Keep this goroutine around so that it will appear in the crash dump.
				} else {
					panic(e)
				}
			} else if err == errGoexit {
				// Already in the process of goexit, no need to call again
			} else {
				// Normal return
				for _, ch := range c.chans {
					ch <- Result{c.val, c.err, c.dups > 0, c.empty}
				}
			}
		}
	}()

	func() {
		defer func() {
			if !normalReturn {
				// Ideally, we would wait to take a stack trace until we've determined
				// whether this is a panic or a runtime.Goexit.
				//
				// Unfortunately, the only way we can distinguish the two is to see
				// whether the recover stopped the goroutine from terminating, and by
				// the time we know that, the part of the stack trace relevant to the
				// panic has been discarded.
				if r := recover(); r != nil {
					err = newPanicError(r)
				}
			}
		}()

		ret, err = g.CommitFunc(taskParams)
		for _, key := range taskParams {
			c := g.m[key]
			val, exist := ret[key]
			c.err, c.val, c.empty = err, val, !exist
		}
		normalReturn = true
	}()

	if !normalReturn {
		recovered = true
	}
}

