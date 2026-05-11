package main
import (
	"encoding/json"
	"fmt"
)
type E struct { Name string }
func (e *E) UnmarshalJSON(data []byte) error {
	fmt.Println("E.UnmarshalJSON called")
	e.Name = "unmarshaled"
	return nil
}
type T struct { E }
func (t *T) UnmarshalJSON(data []byte) error {
	fmt.Println("T.UnmarshalJSON called")
	type Alias T
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil { return err }
	t.E = a.E
	return nil
}
func main() {
	var t T
	json.Unmarshal([]byte("{\"Name\":\"test\"}"), &t)
	fmt.Println(t.Name)
}
