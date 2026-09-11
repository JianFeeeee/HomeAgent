package sdk

import "gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"

type knowledgeImpl struct{ ks *knowledge.Store }

func NewKnowledge(ks *knowledge.Store) KnowledgeAPI { return &knowledgeImpl{ks: ks} }

func (k *knowledgeImpl) Search(query string, topK int) ([]*Knowledge, error) {
	if k.ks == nil {
		return nil, nil
	}
	got := k.ks.Search(query, topK)
	out := make([]*Knowledge, len(got))
	for i, item := range got {
		out[i] = &Knowledge{Name: item.Name, Content: item.Content}
	}
	return out, nil
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

func (k *knowledgeImpl) Stats() map[string]interface{} {
	if k.ks == nil {
		return map[string]interface{}{}
	}
	return k.ks.Stats()
}

func (k *knowledgeImpl) Remove(name string) error {
	if k.ks == nil {
		return nil
	}
	return k.ks.Remove(name)
}

var _ KnowledgeAPI = (*knowledgeImpl)(nil)
