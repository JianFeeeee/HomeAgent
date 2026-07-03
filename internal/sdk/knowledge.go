package sdk

import "gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"

type KnowledgeAPI interface {
	Search(query string, topK int) ([]*knowledge.Knowledge, error)
	Add(name, content string) error
	List() ([]string, error)
}

type knowledgeImpl struct {
	ks *knowledge.Store
}

func NewKnowledge(ks *knowledge.Store) KnowledgeAPI {
	return &knowledgeImpl{ks: ks}
}

func (k *knowledgeImpl) Search(query string, topK int) ([]*knowledge.Knowledge, error) {
	if k.ks == nil {
		return nil, nil
	}
	return k.ks.Search(query, topK), nil
}

func (k *knowledgeImpl) Add(name, content string) error {
	if k.ks == nil {
		return nil
	}
	return k.ks.Add(name, content)
}

func (k *knowledgeImpl) List() ([]string, error) {
	if k.ks == nil {
		return nil, nil
	}
	return k.ks.List(), nil
}
