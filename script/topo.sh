 sudo ip netns add hsns
 sudo ip link add veth_vpp1 type veth peer name vpp1
 sudo ip link add veth_vpp2 type veth peer name vpp2
 sudo ip link set dev vpp1 up
 sudo ip link set dev vpp2 up
 sudo ip link set dev veth_vpp1 up netns hsns
 sudo ip link set dev veth_vpp2 up netns hsns


 sudo ip netns exec hsns brctl addbr hsbr
 sudo ip netns exec hsns brctl addif hsbr veth_vpp1
 sudo ip netns exec hsns brctl addif hsbr veth_vpp2
 sudo ip netns exec hsns ifconfig hsbr up

