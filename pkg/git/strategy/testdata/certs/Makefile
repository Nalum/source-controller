# Copyright 2021 The Flux authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

all: server-key.pem

ca-key.pem: ca-csr.json
	cfssl gencert -initca ca-csr.json | cfssljson -bare ca –
ca.pem: ca-key.pem
ca.csr: ca-key.pem

server-key.pem: server-csr.json ca-config.json ca-key.pem
	cfssl gencert \
		-ca=ca.pem \
		-ca-key=ca-key.pem \
		-config=ca-config.json \
		-profile=web-servers \
		server-csr.json | cfssljson -bare server
sever.pem: server-key.pem
server.csr: server-key.pem
