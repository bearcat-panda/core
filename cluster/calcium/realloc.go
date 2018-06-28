package calcium

import (
	"context"
	"fmt"
	"sync"

	enginecontainer "github.com/docker/docker/api/types/container"
	"github.com/projecteru2/core/scheduler"
	"github.com/projecteru2/core/types"
	log "github.com/sirupsen/logrus"
)

//ReallocResource allow realloc container resource
func (c *Calcium) ReallocResource(ctx context.Context, IDs []string, cpu float64, mem int64) (chan *types.ReallocResourceMessage, error) {
	// TODO 大量容器 Get 的时候有性能问题
	containers, err := c.store.GetContainers(ctx, IDs)
	if err != nil {
		return nil, err
	}

	// Pod-Node-Containers 三元组
	containersInfo := map[*types.Pod]NodeContainers{}
	// Pod-cpu-node-containers 四元组
	cpuContainersInfo := map[*types.Pod]CPUNodeContainers{}
	// Raw resource container Node-Containers
	rawResourceContainers := NodeContainers{}
	nodeCache := map[string]*types.Node{}

	for _, container := range containers {
		if _, ok := nodeCache[container.Nodename]; !ok {
			node, err := c.GetNode(ctx, container.Podname, container.Nodename)
			if err != nil {
				return nil, err
			}
			nodeCache[container.Nodename] = node
		}
		node := nodeCache[container.Nodename]

		if container.RawResource {
			if _, ok := rawResourceContainers[node]; !ok {
				rawResourceContainers[node] = []*types.Container{}
			}
			rawResourceContainers[node] = append(rawResourceContainers[node], container)
			continue
		}

		pod, err := c.store.GetPod(ctx, container.Podname)
		if err != nil {
			return nil, err
		}

		if _, ok := containersInfo[pod]; !ok {
			containersInfo[pod] = NodeContainers{}
		}
		if _, ok := containersInfo[pod][node]; !ok {
			containersInfo[pod][node] = []*types.Container{}
		}
		containersInfo[pod][node] = append(containersInfo[pod][node], container)

		if pod.Favor == scheduler.CPU_PRIOR {
			if _, ok := cpuContainersInfo[pod]; !ok {
				cpuContainersInfo[pod] = CPUNodeContainers{}
			}
			podCPUContainersInfo := cpuContainersInfo[pod]
			newCPURequire := calculateCPUUsage(c.config.Scheduler.ShareBase, container) + cpu
			if newCPURequire < 0.0 {
				return nil, fmt.Errorf("Cpu can not below zero")
			}
			if _, ok := podCPUContainersInfo[newCPURequire]; !ok {
				podCPUContainersInfo[newCPURequire] = NodeContainers{}
				podCPUContainersInfo[newCPURequire][node] = []*types.Container{}
			}
			podCPUContainersInfo[newCPURequire][node] = append(podCPUContainersInfo[newCPURequire][node], container)
		}
	}

	ch := make(chan *types.ReallocResourceMessage)
	go func() {
		defer close(ch)
		wg := sync.WaitGroup{}
		wg.Add(len(containersInfo) + len(rawResourceContainers))

		// deal with container with raw resource
		for node, containers := range rawResourceContainers {
			go func(node *types.Node, containers []*types.Container) {
				defer wg.Done()
				c.doUpdateContainerWithMemoryPrior(ctx, ch, node.Podname, node, containers, cpu, mem)
			}(node, containers)
		}

		// deal with normal container
		for pod, nodeContainers := range containersInfo {
			if pod.Favor == scheduler.CPU_PRIOR {
				nodeCPUContainersInfo := cpuContainersInfo[pod]
				go func(pod *types.Pod, nodeCPUContainersInfo CPUNodeContainers) {
					defer wg.Done()
					c.reallocContainersWithCPUPrior(ctx, ch, pod, nodeCPUContainersInfo, cpu, mem)
				}(pod, nodeCPUContainersInfo)
				continue
			}
			go func(pod *types.Pod, nodeContainers NodeContainers) {
				defer wg.Done()
				c.reallocContainerWithMemoryPrior(ctx, ch, pod, nodeContainers, cpu, mem)
			}(pod, nodeContainers)
		}
		wg.Wait()
	}()
	return ch, nil
}

func (c *Calcium) checkNodesMemory(ctx context.Context, podname string, nodeContainers NodeContainers, memory int64) error {
	lock, err := c.Lock(ctx, podname, c.config.LockTimeout)
	if err != nil {
		return err
	}
	defer lock.Unlock(ctx)
	for node, containers := range nodeContainers {
		if cap := int(node.MemCap / memory); cap < len(containers) {
			return fmt.Errorf("Not enough resource %s", node.Name)
		}
		if err := c.store.UpdateNodeMem(ctx, podname, node.Name, int64(len(containers))*memory, "-"); err != nil {
			return err
		}
	}
	return nil
}

func (c *Calcium) reallocContainerWithMemoryPrior(
	ctx context.Context,
	ch chan *types.ReallocResourceMessage,
	pod *types.Pod,
	nodeContainers NodeContainers,
	cpu float64, memory int64) {

	if memory > 0 {
		if err := c.checkNodesMemory(ctx, pod.Name, nodeContainers, memory); err != nil {
			log.Errorf("[reallocContainerWithMemoryPrior] realloc memory failed %v", err)
			for _, containers := range nodeContainers {
				for _, container := range containers {
					ch <- &types.ReallocResourceMessage{ContainerID: container.ID, Success: false}
				}
			}
			return
		}
	}

	// 不并发操作了
	for node, containers := range nodeContainers {
		c.doUpdateContainerWithMemoryPrior(ctx, ch, pod.Name, node, containers, cpu, memory)
	}
}

func (c *Calcium) doUpdateContainerWithMemoryPrior(
	ctx context.Context,
	ch chan *types.ReallocResourceMessage,
	podname string,
	node *types.Node,
	containers []*types.Container,
	cpu float64, memory int64) {

	for _, container := range containers {
		containerJSON, err := container.Inspect(ctx)
		if err != nil {
			log.Errorf("[doUpdateContainerWithMemoryPrior] get container failed %v", err)
			ch <- &types.ReallocResourceMessage{ContainerID: containerJSON.ID, Success: false}
			continue
		}

		cpuQuota := int64(cpu * float64(CpuPeriodBase))
		newCPUQuota := containerJSON.HostConfig.CPUQuota + cpuQuota
		newMemory := containerJSON.HostConfig.Memory + memory
		if newCPUQuota <= 0 || newMemory <= minMemory {
			log.Warnf("[doUpdateContainerWithMemoryPrior] new resource invaild %s, %d, %d", containerJSON.ID, newCPUQuota, newMemory)
			ch <- &types.ReallocResourceMessage{ContainerID: containerJSON.ID, Success: false}
			continue
		}
		newCPU := float64(newCPUQuota) / float64(CpuPeriodBase)
		log.Debugf("[doUpdateContainerWithMemoryPrior] quota:%d, cpu: %f, mem: %d", newCPUQuota, newCPU, newMemory)

		// CPUQuota not cpu
		newResource := makeMemoryPriorSetting(newMemory, newCPU)
		updateConfig := enginecontainer.UpdateConfig{Resources: newResource}
		if err := reSetContainer(ctx, containerJSON.ID, node, updateConfig); err != nil {
			log.Errorf("[doUpdateContainerWithMemoryPrior] update container failed %v, %s", err, containerJSON.ID)
			ch <- &types.ReallocResourceMessage{ContainerID: containerJSON.ID, Success: false}
			// 如果是增加内存，失败的时候应该把内存还回去
			if memory > 0 && !container.RawResource {
				if err := c.store.UpdateNodeMem(ctx, podname, node.Name, memory, "+"); err != nil {
					log.Errorf("[doUpdateContainerWithMemoryPrior] failed to set mem back %s", containerJSON.ID)
				}
			}
			continue
		}
		// 如果是要降低内存，当执行成功的时候需要把内存还回去
		if memory < 0 && !container.RawResource {
			if err := c.store.UpdateNodeMem(ctx, podname, node.Name, -memory, "+"); err != nil {
				log.Errorf("[doUpdateContainerWithMemoryPrior] failed to set mem back %s", containerJSON.ID)
			}
		}

		container.Memory = newMemory
		if err := c.store.AddContainer(ctx, container); err != nil {
			log.Errorf("[doUpdateContainerWithMemoryPrior] update container meta failed %v", err)
			// 立即中断
			ch <- &types.ReallocResourceMessage{ContainerID: container.ID, Success: false}
			return
		}
		ch <- &types.ReallocResourceMessage{ContainerID: containerJSON.ID, Success: true}
	}
}

func (c *Calcium) reallocNodesCPU(
	ctx context.Context,
	podname string,
	nodesInfoMap CPUNodeContainers,
) (CPUNodeContainersMap, error) {

	lock, err := c.Lock(ctx, podname, c.config.LockTimeout)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock(ctx)

	// TODO too slow
	nodesCPUMap := CPUNodeContainersMap{}
	for cpu, nodesInfo := range nodesInfoMap {
		for node, containers := range nodesInfo {
			for _, container := range containers {
				// 把 CPU 还回去，变成新的可用资源
				// 即便有并发操作，不影响 Create 操作
				// 最坏情况就是 CPU 重叠了，可以外部纠正
				if err := c.store.UpdateNodeCPU(ctx, podname, node.Name, container.CPU, "+"); err != nil {
					return nil, err
				}
				node.CPU.Add(container.CPU)
			}

			// 按照 Node one by one 重新计算可以部署多少容器
			containersNum := len(containers)
			nodesInfo := []types.NodeInfo{
				types.NodeInfo{
					CPUAndMem: types.CPUAndMem{
						CpuMap: node.CPU,
						MemCap: 0,
					},
					Name: node.Name,
				},
			}

			result, changed, err := c.scheduler.SelectCPUNodes(nodesInfo, cpu, containersNum)
			if err != nil {
				for _, container := range containers {
					if err := c.store.UpdateNodeCPU(ctx, podname, node.Name, container.CPU, "-"); err != nil {
						return nil, err
					}
				}
				return nil, err
			}

			nodeCPUMap, isChanged := changed[node.Name]
			containersCPUMap, hasResult := result[node.Name]
			if isChanged && hasResult {
				node.CPU = nodeCPUMap
				if err := c.store.UpdateNode(ctx, node); err != nil {
					return nil, err
				}
				if _, ok := nodesCPUMap[cpu]; !ok {
					nodesCPUMap[cpu] = NodeCPUMap{}
				}
				nodesCPUMap[cpu][node] = containersCPUMap
			}
		}
	}
	return nodesCPUMap, nil
}

// mem not used in this prior
func (c *Calcium) reallocContainersWithCPUPrior(
	ctx context.Context,
	ch chan *types.ReallocResourceMessage,
	pod *types.Pod,
	nodesInfoMap CPUNodeContainers,
	cpu float64, memory int64) {

	nodesCPUMap, err := c.reallocNodesCPU(ctx, pod.Name, nodesInfoMap)
	if err != nil {
		log.Errorf("[reallocContainersWithCPUPrior] realloc cpu resource failed %v", err)
		for _, nodeInfoMap := range nodesInfoMap {
			for _, containers := range nodeInfoMap {
				for _, container := range containers {
					ch <- &types.ReallocResourceMessage{ContainerID: container.ID, Success: false}
				}
			}
		}
		return
	}

	// 不并发操作了
	for cpu, nodesCPUResult := range nodesCPUMap {
		c.doReallocContainersWithCPUPrior(ctx, ch, pod.Name, nodesCPUResult, nodesInfoMap[cpu])
	}
}

func (c *Calcium) doReallocContainersWithCPUPrior(
	ctx context.Context,
	ch chan *types.ReallocResourceMessage,
	podname string,
	nodesCPUResult NodeCPUMap,
	nodesInfoMap NodeContainers,
) {

	for node, cpuset := range nodesCPUResult {
		containers := nodesInfoMap[node]
		for index, container := range containers {
			//TODO 如果需要限制内存，需要在这里 inspect 一下
			quota := cpuset[index]
			resource := makeCPUPriorSetting(c.config.Scheduler.ShareBase, quota)
			updateConfig := enginecontainer.UpdateConfig{Resources: resource}
			if err := reSetContainer(ctx, container.ID, node, updateConfig); err != nil {
				log.Errorf("[doReallocContainersWithCPUPrior] update container failed %v", err)
				// TODO 这里理论上是可以恢复 CPU 占用表的，一来我们知道新的占用是怎样，二来我们也晓得老的占用是啥样
				ch <- &types.ReallocResourceMessage{ContainerID: container.ID, Success: false}
			}

			container.CPU = quota
			if err := c.store.AddContainer(ctx, container); err != nil {
				log.Errorf("[doReallocContainersWithCPUPrior] update container meta failed %v", err)
				// 立即中断
				ch <- &types.ReallocResourceMessage{ContainerID: container.ID, Success: false}
				return
			}
			ch <- &types.ReallocResourceMessage{ContainerID: container.ID, Success: true}
		}
	}
}
