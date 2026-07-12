// Package skill 实现 Skill 注册表（30 个内置 skill）+ Agent ↔ Skill 关联管理。
// 子轮 2.3 引入。任务驱动推荐（子轮 2.4）通过 Service.RecommendAgentsBySkills 调用。
package skill

import "github.com/google/uuid"

// SkillItem 单个 skill 的对外 DTO。
type SkillItem struct {
	ID           string                      `json:"id"`
	Category     string                      `json:"category"`
	Name         string                      `json:"name"`
	Description  string                      `json:"description"`
	SortOrder    int32                       `json:"sort_order"`
	Translations map[string]SkillTranslation `json:"translations,omitempty"`
}

// SkillTranslation is locale-specific public copy for one canonical Skill.
// The top-level fields remain the catalog's default copy for compatibility.
type SkillTranslation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SkillListResponse 是公开 Skill 目录列表响应。
type SkillListResponse struct {
	Items          []SkillItem `json:"items"`
	Total          int64       `json:"total"`
	Page           int32       `json:"page"`
	Size           int32       `json:"size"`
	Query          string      `json:"query,omitempty"`
	CategoryFilter string      `json:"category_filter,omitempty"`
	Sort           string      `json:"sort"`
}

// SetSkillsRequest 创作者绑定 skill 列表。
type SetSkillsRequest struct {
	// SkillIDs Agent 声明的 skill_id 列表，最多 5 个；重复 / 空串视为非法。
	SkillIDs []string `json:"skill_ids" validate:"required"`
}

// SetSkillsResponse 绑定后回写最新列表。
type SetSkillsResponse struct {
	AgentID string      `json:"agent_id"`
	Items   []SkillItem `json:"items"`
}

// CreateSkillProposalRequest 是用户提交缺失 Skill / 导入声明后的提案请求。
type CreateSkillProposalRequest struct {
	AgentID         *string `json:"agent_id,omitempty"`
	ProposedSkillID string  `json:"proposed_skill_id" validate:"required,min=3,max=120"`
	Category        string  `json:"category" validate:"required,min=2,max=80"`
	Name            string  `json:"name" validate:"required,min=1,max=120"`
	Description     string  `json:"description" validate:"required,min=4,max=1000"`
	Source          string  `json:"source,omitempty" validate:"omitempty,oneof=manual imported_text imported_json"`
}

// SkillProposalItem 是 Skill Proposal 的对外 DTO。
type SkillProposalItem struct {
	ID              string  `json:"id"`
	AgentID         *string `json:"agent_id,omitempty"`
	ProposedSkillID string  `json:"proposed_skill_id"`
	Category        string  `json:"category"`
	Name            string  `json:"name"`
	Description     string  `json:"description"`
	Source          string  `json:"source"`
	Status          string  `json:"status"`
	MatchedSkillID  *string `json:"matched_skill_id,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// SkillProposalListResponse 是创作者侧提案列表。
type SkillProposalListResponse struct {
	Items        []SkillProposalItem `json:"items"`
	Total        int64               `json:"total"`
	Page         int32               `json:"page"`
	Size         int32               `json:"size"`
	Query        string              `json:"query,omitempty"`
	StatusFilter string              `json:"status_filter,omitempty"`
	Sort         string              `json:"sort"`
}

// AgentMatch 任务驱动推荐结果（供 2.4 task 模块直接使用）。
//
// MatchCount 是输入 skill 中命中的数量，用于排序与"匹配度"展示；
// VerifiedCount 是命中 skill 中已 verified 的子集，用于"可信度"加权（模块 B）；
// TotalCalls 用作热度 tie-break。
type AgentMatch struct {
	AgentID       uuid.UUID
	MatchCount    int32
	VerifiedCount int32
	TotalCalls    int32
}
