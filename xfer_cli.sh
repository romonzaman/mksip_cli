./sipclient <<'EOF'
dial 1001 1
wait 1 CONNECTED 8000
hold 1
dial 1002 2
wait 2 CONNECTED 8000
xfer 1 2
quit
EOF
