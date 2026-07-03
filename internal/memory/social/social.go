package social

import (
	"fmt"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

const (
	entityTypePerson = "person"
	entityTypeTrait  = "trait_value"
	traitPrefix      = "trait:"
)

type PersonProfile struct {
	Name      string                    `json:"name"`
	Traits    map[string]string         `json:"traits,omitempty"`
	Relations []SocialRelation          `json:"relations,omitempty"`
}

type SocialRelation struct {
	Person   string `json:"person"`
	Relation string `json:"relation"` // 关系类型：朋友/家人/同事/邻居/...
}

type SocialStore struct {
	db *memory.GraphDB
	mu sync.RWMutex
}

func New(db *memory.GraphDB) *SocialStore {
	return &SocialStore{db: db}
}

// GetPerson 获取人物完整档案（特质 + 社交关系）
func (s *SocialStore) GetPerson(name string) (*PersonProfile, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	result, err := s.db.Recall([]string{name}, nil, 2, "")
	if err != nil {
		return nil, err
	}

	profile := &PersonProfile{
		Name:   name,
		Traits: make(map[string]string),
	}

	// 查找指定 person 的 ID
	var personID int64
	for _, e := range result.Entities {
		if e.Name == name {
			personID = e.ID
			break
		}
	}
	if personID == 0 {
		return nil, fmt.Errorf("person '%s' not found", name)
	}

	// 区分 trait 关系和社交关系
	for _, r := range result.Relations {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			// 特质：trait:<特质名>
			traitName := strings.TrimPrefix(r.RelationType, traitPrefix)
			if r.SourceID == personID {
				profile.Traits[traitName] = r.TargetName
			} else {
				profile.Traits[traitName] = r.SourceName
			}
		} else if r.SourceID == personID {
			profile.Relations = append(profile.Relations, SocialRelation{
				Person:   r.TargetName,
				Relation: r.RelationType,
			})
		} else if r.TargetID == personID {
			profile.Relations = append(profile.Relations, SocialRelation{
				Person:   r.SourceName,
				Relation: r.RelationType,
			})
		}
	}

	return profile, nil
}

// SetTrait 设置/更新人物特质。如果同名特质已存在则覆盖
func (s *SocialStore) SetTrait(name, trait, value string) error {
	if s.db == nil {
		return fmt.Errorf("social store not available")
	}

	// 先清除旧特质值
	oldVal, found := s.GetTrait(name, trait)
	if found && oldVal != "" {
		s.db.Purge(map[string]string{
			"subject_contains": name,
			"relation_type":   traitPrefix + trait,
		}, "soft")
	}

	triples := []memory.Triple{
		{
			Subject:      name,
			SubjectType:  entityTypePerson,
			Relation:     traitPrefix + trait,
			Object:       value,
			ObjectType:   entityTypeTrait,
			Confidence:   1.0,
		},
	}
	_, _, err := s.db.Commit(triples, "social_trait", 0)
	return err
}

// GetTrait 获取指定人物的指定特质值
func (s *SocialStore) GetTrait(name, trait string) (string, bool) {
	if s.db == nil {
		return "", false
	}

	result, err := s.db.Recall([]string{name}, nil, 1, "")
	if err != nil || result == nil {
		return "", false
	}

	var personID int64
	for _, e := range result.Entities {
		if e.Name == name {
			personID = e.ID
			break
		}
	}
	if personID == 0 {
		return "", false
	}

	for _, r := range result.Relations {
		if r.RelationType == traitPrefix+trait {
			if r.SourceID == personID {
				return r.TargetName, true
			}
			return r.SourceName, true
		}
	}
	return "", false
}

// AddRelation 建立两人之间的社交关系
func (s *SocialStore) AddRelation(personA, relation, personB string) error {
	if s.db == nil {
		return fmt.Errorf("social store not available")
	}

	triples := []memory.Triple{
		{
			Subject:      personA,
			SubjectType:  entityTypePerson,
			Relation:     relation,
			Object:       personB,
			ObjectType:   entityTypePerson,
			Confidence:   1.0,
		},
	}
	_, _, err := s.db.Commit(triples, "social_relation", 0)
	return err
}

// RemoveRelation 删除两人之间的社交关系
func (s *SocialStore) RemoveRelation(personA, relation, personB string) error {
	if s.db == nil {
		return fmt.Errorf("social store not available")
	}

	_, err := s.db.Purge(map[string]string{
		"subject_contains": personA,
		"target_contains":  personB,
		"relation_type":    relation,
	}, "soft")
	return err
}

// GetRelations 获取指定人物的所有社交关系
func (s *SocialStore) GetRelations(name string) ([]SocialRelation, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	result, err := s.db.Recall([]string{name}, nil, 1, "")
	if err != nil {
		return nil, err
	}

	var personID int64
	for _, e := range result.Entities {
		if e.Name == name {
			personID = e.ID
			break
		}
	}
	if personID == 0 {
		return nil, nil
	}

	var relations []SocialRelation
	for _, r := range result.Relations {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			continue
		}
		if r.SourceID == personID {
			relations = append(relations, SocialRelation{Person: r.TargetName, Relation: r.RelationType})
		} else if r.TargetID == personID {
			relations = append(relations, SocialRelation{Person: r.SourceName, Relation: r.RelationType})
		}
	}
	return relations, nil
}

// GetNetwork 获取指定人物周围 depth 度的社交网络
func (s *SocialStore) GetNetwork(name string, depth int) ([]*PersonProfile, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	// 用 Recall 的 BFS 遍历获取多度关联
	result, err := s.db.Recall([]string{name}, nil, depth, "")
	if err != nil {
		return nil, err
	}

	personMap := make(map[int64]*PersonProfile)
	for _, e := range result.Entities {
		p := &PersonProfile{
			Name:   e.Name,
			Traits: make(map[string]string),
		}
		personMap[e.ID] = p
	}

	for _, r := range result.Relations {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			traitName := strings.TrimPrefix(r.RelationType, traitPrefix)
			if p, ok := personMap[r.SourceID]; ok {
				p.Traits[traitName] = r.TargetName
			}
			if p, ok := personMap[r.TargetID]; ok {
				p.Traits[traitName] = r.SourceName
			}
		}
	}

	// 收集关系
	for _, r := range result.Relations {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			continue
		}
		sr := SocialRelation{Relation: r.RelationType}
		if p, ok := personMap[r.SourceID]; ok {
			sr.Person = r.TargetName
			p.Relations = append(p.Relations, sr)
		}
		sr = SocialRelation{Relation: r.RelationType}
		if p, ok := personMap[r.TargetID]; ok {
			sr.Person = r.SourceName
			p.Relations = append(p.Relations, sr)
		}
	}

	var profiles []*PersonProfile
	for _, p := range personMap {
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// ListPersons 列出所有已知人物（entity.type = person）
func (s *SocialStore) ListPersons() ([]string, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	result, err := s.db.Recall(nil, nil, 1, "")
	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range result.Entities {
		if e.Type == entityTypePerson || e.Type == "Person" {
			names = append(names, e.Name)
		}
	}
	return names, nil
}
