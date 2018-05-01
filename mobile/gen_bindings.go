// +build ignore

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"text/template"

	"github.com/golang/glog"
	"github.com/grpc-ecosystem/grpc-gateway/codegenerator"
	"github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway/descriptor"
)

var (
	importPrefix         = flag.String("import_prefix", "", "prefix to be added to go package paths for imported proto files")
	importPath           = flag.String("import_path", "", "used as the package if no input files declare go_package. If it contains slashes, everything up to the rightmost slash is ignored.")
	useRequestContext    = flag.Bool("request_context", true, "determine whether to use http.Request's context or not")
	allowDeleteBody      = flag.Bool("allow_delete_body", false, "unless set, HTTP DELETE methods may not have a body")
	grpcAPIConfiguration = flag.String("grpc_api_configuration", "", "path to gRPC API Configuration in YAML format")
)

func main() {
	flag.Parse()
	defer glog.Flush()

	reg := descriptor.NewRegistry()

	req, err := codegenerator.ParseRequest(os.Stdin)

	if err != nil {
		fmt.Println(err)
		return
	}

	reg.SetPrefix(*importPrefix)
	reg.SetImportPath(*importPath)
	reg.SetAllowDeleteBody(*allowDeleteBody)
	if err := reg.Load(req); err != nil {
		fmt.Println("err loading: ", err)
		return
	}

	var targets []*descriptor.File
	for _, target := range req.FileToGenerate {
		f, err := reg.LookupFile(target)
		if err != nil {
			glog.Fatal(err)
		}
		targets = append(targets, f)
	}

	params := struct {
		Name    string
		Package string
		Number  int
	}{
		"Johan",
		"lndmobile",
		111,
	}

	f, err := os.Create("./api_generated.go")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer f.Close()

	wr := bufio.NewWriter(f)
	defer wr.Flush()

	//w := bytes.NewBuffer(nil)
	if err := headerTemplate.Execute(wr, params); err != nil {
		fmt.Println(err)
		return
	}

	var services []*descriptor.Service

	for _, target := range targets {
		//		wr.WriteString(fmt.Sprintf("name: %s\n", target.GoPkg))
		for _, s := range target.Services {
			services = append(services, s)
		}
	}

	for _, s := range services {
		if s.GetName() != "Lightning" {
			continue
		}
		//		wr.WriteString(fmt.Sprintf("\tservice: %s (%d)\n", s.GetName(), len(s.Methods)))
		for _, m := range s.Methods {
			if m.GetName() == "GetInfo" {
				continue
			}
			if m.GetName() == "SubscribeInvoices" {
				continue
			}
			if m.GetName() == "SendPayment" {
				continue
			}

			//			wr.WriteString(fmt.Sprintf("\tmethod: %s\n", m.GetName()))
			clientStream := false
			serverStream := false
			if m.ClientStreaming != nil {
				clientStream = *m.ClientStreaming
			}

			if m.ServerStreaming != nil {
				serverStream = *m.ServerStreaming
			}

			switch {
			case !clientStream && !serverStream:

				//wr.WriteString(fmt.Sprintf("creating once handler\n"))
				p := onceParams{
					MethodName:  m.GetName(),
					RequestType: m.GetInputType()[1:],
				}

				if err := onceTemplate.Execute(wr, p); err != nil {
					fmt.Println(err)
					return
				}
			}

		}

	}
}

type onceParams struct {
	MethodName  string
	RequestType string
}

var (
	headerTemplate = template.Must(template.New("header").Parse(`
// Code generated by protoc-gen-grpc-gateway. DO NOT EDIT.
// source: {{.Name}}
package {{.Package}}

import (
	"context"

	"github.com/golang/protobuf/proto"
	"github.com/lightningnetwork/lnd/lnrpc"
)
`))

	onceTemplate = template.Must(template.New("once").Parse(`
func {{.MethodName}}(msg []byte, callback Callback) {
	s := &onceHandler{
		newProto: func() proto.Message {
			return &{{.RequestType}}{}
		},
		getSync: func(ctx context.Context, client lnrpc.LightningClient,
			req proto.Message) (proto.Message, error) {
			r := req.(*{{.RequestType}})
			return client.{{.MethodName}}(ctx, r)
		},
	}
	s.start(msg, callback)
}
`))
)
