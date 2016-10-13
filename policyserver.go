package main

import (
	//"fmt"
	"encoding/json"
	"fmt"
	"net/http"
	//"os"
	//"os/exec"
	"strings"

	"github.com/cloudfoundry-community/go-cfclient"
	//"github.com/urfave/cli"
)

type portGroup struct {
	part1 string
	part2 string
}

var endpointGroup map[string]portGroup

var policytoGroup map[string]string

type SpaceGroup struct {
	Space    string `json:"space"`
	Endpoint string `json:"endpoint"`
}

func pol(w http.ResponseWriter, r *http.Request) {
	//need to have space id for later association
}

func rule(w http.ResponseWriter, r *http.Request) {

}

func sg(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		defer func() {
			if r := recover(); r != nil {
				w.Write([]byte("0"))
			}
		}()
		//todo: no talking with pg apis, find name from cf and parse it
		space := r.URL.Query().Get("space")
		fmt.Println(space)
		policy := GetPolicy(space)
		res := policytoGroup[policy]
		if res == "" {
			res = "0"
		}
		fmt.Println("sending back group id")
		w.Write([]byte(res))
		//http.Error(w, "cannot find matching space", http.StatusBadRequest)
		return

	case "POST":
		var req SpaceGroup
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Endpoint == "" || req.Space == "" {
			http.Error(w, "post data error", http.StatusBadRequest)
			return
		}
		policy := GetPolicy(req.Space)

		//PG can handle re-post,policy tag equals endpoint group tag
		// create endpoint using port prefix
		addEndpoint(policy, policy, req.Endpoint)

		fmt.Println("got post data %s, %s", req.Space, req.Endpoint)
	// Create a new record.
	//case "PUT":
	// Update an existing record.
	case "DELETE":
		// Remove the record.
		space := r.URL.Query().Get("space")
		fmt.Println("deleting from pg endpoint %s", space)

		return
	}

}

func addEndPoint(req SpaceGroup) {
	fmt.Println("Recieving endpoint from space %s, endpoint %s", req.Space, req.Endpoint)
}
func addEndPointGroup(req SpaceGroup) {
	fmt.Println("Create endpoint group %s.", req.Space, req.Endpoint)
}

func createSpace() string {
	return "uuid"
}

func createPolicy(name string) {
	//todo: need a floating ip
}

func createEndGroup(name string, policy string) {
	//todo: need the tenant id
}

func addEndpoint(space string, policy string, ipPort string) {

}

func deleteSpace() {

}

func delEndGroup(space string) {
	//delete endpoint group
}

func config() {
	//conatins the hard coded stuff
	policytoGroup["blue"] = "0"
	policytoGroup["green"] = "1"
	policytoGroup["red"] = "2"
	endpointGroup["blue"] = portGroup{part1: "59392/1024", part2: "60416/1024"}
	endpointGroup["green"] = portGroup{part1: "61440/1024", part2: "62464/1024"}
	endpointGroup["red"] = portGroup{part1: "63488/1023", part2: "64512/1023"}
	createPolicy("blue")
	createPolicy("green")
	createPolicy("red")
	createEndGroup("red", "red")
	createEndGroup("green", "green")
	createEndGroup("blue", "blue")
}

func GetPolicy(spaceid string) string {
	c := &cfclient.Config{
		ApiAddress: "https://api.cf.plumgrid.com",
		Username:   "admin",
		Password:   "plumgrid",
	}

	client, _ := cfclient.NewClient(c)
	fmt.println(client == nil)
	spaces, _ := client.ListSpaces()
	for _, s := range spaces {
		if s.Guid == spaceid {
			return parse(strings.ToLower(s.Name))
		}
	}

	return "blue"

}

func parse(name string) string {
	//hard coded
	switch {
	case strings.Contains(name, "dev"):
		return "blue"
	case strings.Contains(name, "int"):
		return "green"
	case strings.Contains(name, "prod"):
		return "red"
	}
	return "blue"

}

func main() {

	policytoGroup = make(map[string]string)
	endpointGroup = make(map[string]portGroup)
	config()
	mux := http.NewServeMux()
	mux.HandleFunc("/spacegroup", sg)
	mux.HandleFunc("/policytag", pol)
	mux.HandleFunc("/rule", rule)
	http.ListenAndServe(":8000", mux)

}
