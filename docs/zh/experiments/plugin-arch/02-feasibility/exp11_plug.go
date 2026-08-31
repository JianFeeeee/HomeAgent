//go:build ignore
package main
import ("bufio";"encoding/json";"os")
type Req struct{ ID int `json:"id"`; Method string `json:"method"`; Args json.RawMessage `json:"args"` }
type Res struct{ ID int `json:"id"`; Result string `json:"result"` }
func main(){
	dec:=json.NewDecoder(bufio.NewReader(os.Stdin))
	w:=bufio.NewWriter(os.Stdout); enc:=json.NewEncoder(w)
	for { var q Req
		if err:=dec.Decode(&q); err!=nil {return}
		enc.Encode(Res{ID:q.ID, Result:`{"ok":true,"data":"` + string(q.Args) + `"}`}); w.Flush() }
}
