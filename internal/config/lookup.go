package config

import "fmt"

// TaskName 是任务名，用于查模型绑定。
type TaskName string

const (
	TaskPlanner   TaskName = "planner"
	TaskReplyer   TaskName = "replyer"
	TaskUtils     TaskName = "utils"
	TaskLearner   TaskName = "learner"
	TaskVLM       TaskName = "vlm"
	TaskEmbedding TaskName = "embedding"
)

// Provider 按名字查找服务商。
func (c *Config) Provider(name string) (*ProviderConfig, error) {
	for i := range c.LLM.Providers {
		if c.LLM.Providers[i].Name == name {
			return &c.LLM.Providers[i], nil
		}
	}
	return nil, fmt.Errorf("未找到服务商 %q", name)
}

// Model 按名字查找模型。
func (c *Config) Model(name string) (*ModelConfig, error) {
	for i := range c.LLM.Models {
		if c.LLM.Models[i].Name == name {
			return &c.LLM.Models[i], nil
		}
	}
	return nil, fmt.Errorf("未找到模型 %q", name)
}

// Task 返回任务配置。
func (c *Config) Task(name TaskName) TaskModelConfig {
	switch name {
	case TaskPlanner:
		return c.LLM.Tasks.Planner
	case TaskReplyer:
		return c.LLM.Tasks.Replyer
	case TaskUtils:
		return c.LLM.Tasks.Utils
	case TaskLearner:
		return c.LLM.Tasks.Learner
	case TaskVLM:
		return c.LLM.Tasks.VLM
	case TaskEmbedding:
		return c.LLM.Tasks.Embedding
	}
	return c.LLM.Tasks.Utils
}

// ResolveModels 返回任务可用模型列表，按 MaiBot 的回退规则处理空列表。
//
// 回退规则（对应 MaiBot 各任务 model_list 留空时的行为）：
//   - planner 无回退，必须显式配置
//   - replyer 回退到 planner
//   - learner 回退到 utils
//   - vlm 无回退，为空时调用方应报错或降级为纯文本
func (c *Config) ResolveModels(name TaskName) []string {
	task := c.Task(name)
	if len(task.Models) > 0 {
		return task.Models
	}
	switch name {
	case TaskReplyer:
		return c.Task(TaskPlanner).Models
	case TaskLearner:
		return c.Task(TaskUtils).Models
	}
	return nil
}

// PrimaryModel 返回任务的首选模型名。
func (c *Config) PrimaryModel(name TaskName) (string, error) {
	models := c.ResolveModels(name)
	if len(models) == 0 {
		return "", fmt.Errorf("任务 %s 没有可用模型", name)
	}
	return models[0], nil
}

// HasModel 判断某个模型是否已配置。
func (c *Config) HasModel(name string) bool {
	_, err := c.Model(name)
	return err == nil
}
