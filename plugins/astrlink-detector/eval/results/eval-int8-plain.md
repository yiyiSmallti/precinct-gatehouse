
### scope=full threshold=0.70 (scan 3658 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 0.904 | 0.922 | 0.913 | 0.923 | 0.941 | 0.932 |
| zh | private_address | 36 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| zh | account | 37 | 1.000 | 0.973 | 0.986 | 1.000 | 0.973 | 0.986 |
| zh | common_secret | 36 | 1.000 | 0.944 | 0.971 | 1.000 | 0.944 | 0.971 |
| zh | private_date | 34 | 0.651 | 0.824 | 0.727 | 0.791 | 1.000 | 0.883 |
| zh | **macro F1** | | | | **0.920** | | | **0.955** |
| en | private_person | 46 | 0.958 | 1.000 | 0.979 | 0.958 | 1.000 | 0.979 |
| en | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| en | account | 34 | 0.912 | 0.912 | 0.912 | 0.912 | 0.912 | 0.912 |
| en | common_secret | 34 | 0.971 | 0.971 | 0.971 | 0.971 | 0.971 | 0.971 |
| en | private_date | 34 | 0.756 | 0.912 | 0.827 | 0.829 | 1.000 | 0.907 |
| en | **macro F1** | | | | **0.938** | | | **0.954** |
| mixed | private_person | 42 | 1.000 | 0.929 | 0.963 | 1.000 | 0.929 | 0.963 |
| mixed | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| mixed | account | 34 | 0.926 | 0.735 | 0.820 | 0.926 | 0.735 | 0.820 |
| mixed | common_secret | 34 | 0.971 | 1.000 | 0.986 | 0.971 | 1.000 | 0.986 |
| mixed | private_date | 36 | 0.946 | 0.972 | 0.959 | 0.946 | 0.972 | 0.959 |
| mixed | **macro F1** | | | | **0.945** | | | **0.945** |
| all | private_person | 139 | 0.950 | 0.950 | 0.950 | 0.957 | 0.957 | 0.957 |
| all | private_address | 104 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| all | account | 105 | 0.948 | 0.876 | 0.911 | 0.948 | 0.876 | 0.911 |
| all | common_secret | 104 | 0.981 | 0.971 | 0.976 | 0.981 | 0.971 | 0.976 |
| all | private_date | 104 | 0.777 | 0.904 | 0.836 | 0.851 | 0.990 | 0.916 |
| all | **macro F1** | | | | **0.934** | | | **0.952** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 6.0% (5/83) | 3.6% (3/83) | 12.11 (4 spans / 3303 tokens) | 96.9% (188/194) |
| en | 6.3% (6/95) | 2.1% (2/95) | 2.36 (3 spans / 12722 tokens) | 98.4% (179/182) |
| mixed | - | - | - | 94.4% (170/180) |
| all | 6.2% (11/178) | 2.8% (5/178) | 4.37 (7 spans / 16025 tokens) | 96.6% (537/556) |
| negative | 5.4% (9/166) | 3.0% (5/166) | 8.58 (7 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 96.7% (58/60); spans on clean units by label: {account=1 ip_address=6 private_person=6}; non-target labels in positives: {url=2}

### scope=full threshold=0.80 (scan 3658 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 0.922 | 0.922 | 0.922 | 0.922 | 0.922 | 0.922 |
| zh | private_address | 36 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| zh | account | 37 | 1.000 | 0.919 | 0.958 | 1.000 | 0.919 | 0.958 |
| zh | common_secret | 36 | 1.000 | 0.944 | 0.971 | 1.000 | 0.944 | 0.971 |
| zh | private_date | 34 | 0.700 | 0.824 | 0.757 | 0.850 | 1.000 | 0.919 |
| zh | **macro F1** | | | | **0.922** | | | **0.954** |
| en | private_person | 46 | 0.979 | 1.000 | 0.989 | 0.979 | 1.000 | 0.989 |
| en | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| en | account | 34 | 0.967 | 0.853 | 0.906 | 0.967 | 0.853 | 0.906 |
| en | common_secret | 34 | 0.970 | 0.941 | 0.955 | 0.970 | 0.941 | 0.955 |
| en | private_date | 34 | 0.775 | 0.912 | 0.838 | 0.850 | 1.000 | 0.919 |
| en | **macro F1** | | | | **0.938** | | | **0.954** |
| mixed | private_person | 42 | 1.000 | 0.929 | 0.963 | 1.000 | 0.929 | 0.963 |
| mixed | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| mixed | account | 34 | 0.958 | 0.676 | 0.793 | 0.958 | 0.676 | 0.793 |
| mixed | common_secret | 34 | 0.971 | 1.000 | 0.986 | 0.971 | 1.000 | 0.986 |
| mixed | private_date | 36 | 0.946 | 0.972 | 0.959 | 0.946 | 0.972 | 0.959 |
| mixed | **macro F1** | | | | **0.940** | | | **0.940** |
| all | private_person | 139 | 0.964 | 0.950 | 0.957 | 0.964 | 0.950 | 0.957 |
| all | private_address | 104 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| all | account | 105 | 0.977 | 0.819 | 0.891 | 0.977 | 0.819 | 0.891 |
| all | common_secret | 104 | 0.980 | 0.962 | 0.971 | 0.980 | 0.962 | 0.971 |
| all | private_date | 104 | 0.803 | 0.904 | 0.851 | 0.880 | 0.990 | 0.932 |
| all | **macro F1** | | | | **0.934** | | | **0.950** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 6.0% (5/83) | 3.6% (3/83) | 12.11 (4 spans / 3303 tokens) | 95.4% (185/194) |
| en | 5.3% (5/95) | 1.1% (1/95) | 0.79 (1 spans / 12722 tokens) | 96.7% (176/182) |
| mixed | - | - | - | 92.8% (167/180) |
| all | 5.6% (10/178) | 2.2% (4/178) | 3.12 (5 spans / 16025 tokens) | 95.0% (528/556) |
| negative | 4.8% (8/166) | 2.4% (4/166) | 6.13 (5 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 96.7% (58/60); spans on clean units by label: {ip_address=6 private_person=5}; non-target labels in positives: {url=2}

### scope=full threshold=0.90 (scan 3658 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 0.979 | 0.922 | 0.949 | 0.979 | 0.922 | 0.949 |
| zh | private_address | 36 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| zh | account | 37 | 1.000 | 0.838 | 0.912 | 1.000 | 0.838 | 0.912 |
| zh | common_secret | 36 | 1.000 | 0.944 | 0.971 | 1.000 | 0.944 | 0.971 |
| zh | private_date | 34 | 0.778 | 0.824 | 0.800 | 0.944 | 1.000 | 0.971 |
| zh | **macro F1** | | | | **0.927** | | | **0.961** |
| en | private_person | 46 | 0.979 | 1.000 | 0.989 | 0.979 | 1.000 | 0.989 |
| en | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| en | account | 34 | 1.000 | 0.853 | 0.921 | 1.000 | 0.853 | 0.921 |
| en | common_secret | 34 | 1.000 | 0.941 | 0.970 | 1.000 | 0.941 | 0.970 |
| en | private_date | 34 | 0.838 | 0.912 | 0.873 | 0.838 | 0.912 | 0.873 |
| en | **macro F1** | | | | **0.951** | | | **0.951** |
| mixed | private_person | 42 | 1.000 | 0.905 | 0.950 | 1.000 | 0.905 | 0.950 |
| mixed | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| mixed | account | 34 | 1.000 | 0.559 | 0.717 | 1.000 | 0.559 | 0.717 |
| mixed | common_secret | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| mixed | private_date | 36 | 0.972 | 0.972 | 0.972 | 0.972 | 0.972 | 0.972 |
| mixed | **macro F1** | | | | **0.928** | | | **0.928** |
| all | private_person | 139 | 0.985 | 0.942 | 0.963 | 0.985 | 0.942 | 0.963 |
| all | private_address | 104 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| all | account | 105 | 1.000 | 0.752 | 0.859 | 1.000 | 0.752 | 0.859 |
| all | common_secret | 104 | 1.000 | 0.962 | 0.980 | 1.000 | 0.962 | 0.980 |
| all | private_date | 104 | 0.862 | 0.904 | 0.883 | 0.917 | 0.962 | 0.939 |
| all | **macro F1** | | | | **0.937** | | | **0.948** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 3.6% (3/83) | 1.2% (1/83) | 3.03 (1 spans / 3303 tokens) | 93.8% (182/194) |
| en | 5.3% (5/95) | 1.1% (1/95) | 0.79 (1 spans / 12722 tokens) | 94.5% (172/182) |
| mixed | - | - | - | 88.9% (160/180) |
| all | 4.5% (8/178) | 1.1% (2/178) | 1.25 (2 spans / 16025 tokens) | 92.4% (514/556) |
| negative | 3.6% (6/166) | 1.2% (2/166) | 2.45 (2 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 96.7% (58/60); spans on clean units by label: {ip_address=6 private_person=2}; non-target labels in positives: {url=2}

### scope=full threshold=0.95 (scan 3658 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 1.000 | 0.922 | 0.959 | 1.000 | 0.922 | 0.959 |
| zh | private_address | 36 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| zh | account | 37 | 1.000 | 0.811 | 0.896 | 1.000 | 0.811 | 0.896 |
| zh | common_secret | 36 | 1.000 | 0.944 | 0.971 | 1.000 | 0.944 | 0.971 |
| zh | private_date | 34 | 0.824 | 0.824 | 0.824 | 1.000 | 1.000 | 1.000 |
| zh | **macro F1** | | | | **0.930** | | | **0.965** |
| en | private_person | 46 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| en | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| en | account | 34 | 1.000 | 0.794 | 0.885 | 1.000 | 0.794 | 0.885 |
| en | common_secret | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| en | private_date | 34 | 0.939 | 0.912 | 0.925 | 0.939 | 0.912 | 0.925 |
| en | **macro F1** | | | | **0.950** | | | **0.950** |
| mixed | private_person | 42 | 1.000 | 0.905 | 0.950 | 1.000 | 0.905 | 0.950 |
| mixed | private_address | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| mixed | account | 34 | 1.000 | 0.471 | 0.640 | 1.000 | 0.471 | 0.640 |
| mixed | common_secret | 34 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| mixed | private_date | 36 | 1.000 | 0.972 | 0.986 | 1.000 | 0.972 | 0.986 |
| mixed | **macro F1** | | | | **0.915** | | | **0.915** |
| all | private_person | 139 | 1.000 | 0.942 | 0.970 | 1.000 | 0.942 | 0.970 |
| all | private_address | 104 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 | 1.000 |
| all | account | 105 | 1.000 | 0.695 | 0.820 | 1.000 | 0.695 | 0.820 |
| all | common_secret | 104 | 1.000 | 0.942 | 0.970 | 1.000 | 0.942 | 0.970 |
| all | private_date | 104 | 0.922 | 0.904 | 0.913 | 0.980 | 0.962 | 0.971 |
| all | **macro F1** | | | | **0.935** | | | **0.946** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 2.4% (2/83) | 0.0% (0/83) | 0.00 (0 spans / 3303 tokens) | 93.3% (181/194) |
| en | 4.2% (4/95) | 0.0% (0/95) | 0.00 (0 spans / 12722 tokens) | 92.3% (168/182) |
| mixed | - | - | - | 87.2% (157/180) |
| all | 3.4% (6/178) | 0.0% (0/178) | 0.00 (0 spans / 16025 tokens) | 91.0% (506/556) |
| negative | 2.4% (4/166) | 0.0% (0/166) | 0.00 (0 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 95.0% (57/60); spans on clean units by label: {ip_address=6}; non-target labels in positives: {url=2}

### scope=skip_fenced threshold=0.70 (scan 2966 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 0.896 | 0.843 | 0.869 | 0.917 | 0.863 | 0.889 |
| zh | private_address | 36 | 1.000 | 0.889 | 0.941 | 1.000 | 0.889 | 0.941 |
| zh | account | 37 | 1.000 | 0.865 | 0.928 | 1.000 | 0.865 | 0.928 |
| zh | common_secret | 36 | 1.000 | 0.833 | 0.909 | 1.000 | 0.833 | 0.909 |
| zh | private_date | 34 | 0.615 | 0.706 | 0.658 | 0.769 | 0.882 | 0.822 |
| zh | **macro F1** | | | | **0.861** | | | **0.898** |
| en | private_person | 46 | 0.955 | 0.913 | 0.933 | 0.955 | 0.913 | 0.933 |
| en | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| en | account | 34 | 0.964 | 0.794 | 0.871 | 0.964 | 0.794 | 0.871 |
| en | common_secret | 34 | 0.967 | 0.853 | 0.906 | 0.967 | 0.853 | 0.906 |
| en | private_date | 34 | 0.730 | 0.794 | 0.761 | 0.811 | 0.882 | 0.845 |
| en | **macro F1** | | | | **0.882** | | | **0.899** |
| mixed | private_person | 42 | 1.000 | 0.833 | 0.909 | 1.000 | 0.833 | 0.909 |
| mixed | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| mixed | account | 34 | 0.920 | 0.676 | 0.780 | 0.920 | 0.676 | 0.780 |
| mixed | common_secret | 34 | 0.968 | 0.882 | 0.923 | 0.968 | 0.882 | 0.923 |
| mixed | private_date | 36 | 0.939 | 0.861 | 0.899 | 0.939 | 0.861 | 0.899 |
| mixed | **macro F1** | | | | **0.890** | | | **0.890** |
| all | private_person | 139 | 0.945 | 0.863 | 0.902 | 0.953 | 0.871 | 0.910 |
| all | private_address | 104 | 1.000 | 0.885 | 0.939 | 1.000 | 0.885 | 0.939 |
| all | account | 105 | 0.965 | 0.781 | 0.863 | 0.965 | 0.781 | 0.863 |
| all | common_secret | 104 | 0.978 | 0.856 | 0.913 | 0.978 | 0.856 | 0.913 |
| all | private_date | 104 | 0.752 | 0.788 | 0.770 | 0.835 | 0.875 | 0.854 |
| all | **macro F1** | | | | **0.877** | | | **0.896** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 4.8% (4/83) | 3.6% (3/83) | 12.11 (4 spans / 3303 tokens) | 86.6% (168/194) |
| en | 6.3% (6/95) | 2.1% (2/95) | 2.36 (3 spans / 12722 tokens) | 87.4% (159/182) |
| mixed | - | - | - | 84.4% (152/180) |
| all | 5.6% (10/178) | 2.8% (5/178) | 4.37 (7 spans / 16025 tokens) | 86.2% (479/556) |
| negative | 4.8% (8/166) | 3.0% (5/166) | 8.58 (7 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 0.0% (0/60); spans on clean units by label: {account=1 ip_address=5 private_person=6}; non-target labels in positives: {}

### scope=skip_fenced threshold=0.80 (scan 2966 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 0.915 | 0.843 | 0.878 | 0.915 | 0.843 | 0.878 |
| zh | private_address | 36 | 1.000 | 0.889 | 0.941 | 1.000 | 0.889 | 0.941 |
| zh | account | 37 | 1.000 | 0.811 | 0.896 | 1.000 | 0.811 | 0.896 |
| zh | common_secret | 36 | 1.000 | 0.833 | 0.909 | 1.000 | 0.833 | 0.909 |
| zh | private_date | 34 | 0.667 | 0.706 | 0.686 | 0.833 | 0.882 | 0.857 |
| zh | **macro F1** | | | | **0.862** | | | **0.896** |
| en | private_person | 46 | 0.977 | 0.913 | 0.944 | 0.977 | 0.913 | 0.944 |
| en | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| en | account | 34 | 0.962 | 0.735 | 0.833 | 0.962 | 0.735 | 0.833 |
| en | common_secret | 34 | 0.966 | 0.824 | 0.889 | 0.966 | 0.824 | 0.889 |
| en | private_date | 34 | 0.750 | 0.794 | 0.771 | 0.833 | 0.882 | 0.857 |
| en | **macro F1** | | | | **0.875** | | | **0.892** |
| mixed | private_person | 42 | 1.000 | 0.833 | 0.909 | 1.000 | 0.833 | 0.909 |
| mixed | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| mixed | account | 34 | 0.955 | 0.618 | 0.750 | 0.955 | 0.618 | 0.750 |
| mixed | common_secret | 34 | 0.968 | 0.882 | 0.923 | 0.968 | 0.882 | 0.923 |
| mixed | private_date | 36 | 0.939 | 0.861 | 0.899 | 0.939 | 0.861 | 0.899 |
| mixed | **macro F1** | | | | **0.884** | | | **0.884** |
| all | private_person | 139 | 0.960 | 0.863 | 0.909 | 0.960 | 0.863 | 0.909 |
| all | private_address | 104 | 1.000 | 0.885 | 0.939 | 1.000 | 0.885 | 0.939 |
| all | account | 105 | 0.974 | 0.724 | 0.831 | 0.974 | 0.724 | 0.831 |
| all | common_secret | 104 | 0.978 | 0.846 | 0.907 | 0.978 | 0.846 | 0.907 |
| all | private_date | 104 | 0.781 | 0.788 | 0.785 | 0.867 | 0.875 | 0.871 |
| all | **macro F1** | | | | **0.874** | | | **0.891** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 4.8% (4/83) | 3.6% (3/83) | 12.11 (4 spans / 3303 tokens) | 85.1% (165/194) |
| en | 6.3% (6/95) | 2.1% (2/95) | 1.57 (2 spans / 12722 tokens) | 85.7% (156/182) |
| mixed | - | - | - | 82.8% (149/180) |
| all | 5.6% (10/178) | 2.8% (5/178) | 3.74 (6 spans / 16025 tokens) | 84.5% (470/556) |
| negative | 4.8% (8/166) | 3.0% (5/166) | 7.35 (6 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 0.0% (0/60); spans on clean units by label: {account=1 ip_address=5 private_person=5}; non-target labels in positives: {}

### scope=skip_fenced threshold=0.90 (scan 2966 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 0.977 | 0.843 | 0.905 | 0.977 | 0.843 | 0.905 |
| zh | private_address | 36 | 1.000 | 0.889 | 0.941 | 1.000 | 0.889 | 0.941 |
| zh | account | 37 | 1.000 | 0.730 | 0.844 | 1.000 | 0.730 | 0.844 |
| zh | common_secret | 36 | 1.000 | 0.833 | 0.909 | 1.000 | 0.833 | 0.909 |
| zh | private_date | 34 | 0.750 | 0.706 | 0.727 | 0.938 | 0.882 | 0.909 |
| zh | **macro F1** | | | | **0.865** | | | **0.902** |
| en | private_person | 46 | 0.977 | 0.913 | 0.944 | 0.977 | 0.913 | 0.944 |
| en | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| en | account | 34 | 1.000 | 0.735 | 0.847 | 1.000 | 0.735 | 0.847 |
| en | common_secret | 34 | 1.000 | 0.824 | 0.903 | 1.000 | 0.824 | 0.903 |
| en | private_date | 34 | 0.818 | 0.794 | 0.806 | 0.818 | 0.794 | 0.806 |
| en | **macro F1** | | | | **0.888** | | | **0.888** |
| mixed | private_person | 42 | 1.000 | 0.810 | 0.895 | 1.000 | 0.810 | 0.895 |
| mixed | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| mixed | account | 34 | 1.000 | 0.500 | 0.667 | 1.000 | 0.500 | 0.667 |
| mixed | common_secret | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| mixed | private_date | 36 | 0.969 | 0.861 | 0.912 | 0.969 | 0.861 | 0.912 |
| mixed | **macro F1** | | | | **0.870** | | | **0.870** |
| all | private_person | 139 | 0.983 | 0.856 | 0.915 | 0.983 | 0.856 | 0.915 |
| all | private_address | 104 | 1.000 | 0.885 | 0.939 | 1.000 | 0.885 | 0.939 |
| all | account | 105 | 1.000 | 0.657 | 0.793 | 1.000 | 0.657 | 0.793 |
| all | common_secret | 104 | 1.000 | 0.846 | 0.917 | 1.000 | 0.846 | 0.917 |
| all | private_date | 104 | 0.845 | 0.788 | 0.816 | 0.907 | 0.846 | 0.876 |
| all | **macro F1** | | | | **0.876** | | | **0.888** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 2.4% (2/83) | 1.2% (1/83) | 3.03 (1 spans / 3303 tokens) | 83.5% (162/194) |
| en | 5.3% (5/95) | 1.1% (1/95) | 0.79 (1 spans / 12722 tokens) | 83.5% (152/182) |
| mixed | - | - | - | 78.9% (142/180) |
| all | 3.9% (7/178) | 1.1% (2/178) | 1.25 (2 spans / 16025 tokens) | 82.0% (456/556) |
| negative | 3.0% (5/166) | 1.2% (2/166) | 2.45 (2 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 0.0% (0/60); spans on clean units by label: {ip_address=5 private_person=2}; non-target labels in positives: {}

### scope=skip_fenced threshold=0.95 (scan 2966 ms)

| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |
|---|---|---|---|---|---|---|---|---|
| zh | private_person | 51 | 1.000 | 0.843 | 0.915 | 1.000 | 0.843 | 0.915 |
| zh | private_address | 36 | 1.000 | 0.889 | 0.941 | 1.000 | 0.889 | 0.941 |
| zh | account | 37 | 1.000 | 0.703 | 0.825 | 1.000 | 0.703 | 0.825 |
| zh | common_secret | 36 | 1.000 | 0.833 | 0.909 | 1.000 | 0.833 | 0.909 |
| zh | private_date | 34 | 0.800 | 0.706 | 0.750 | 1.000 | 0.882 | 0.938 |
| zh | **macro F1** | | | | **0.868** | | | **0.906** |
| en | private_person | 46 | 1.000 | 0.913 | 0.955 | 1.000 | 0.913 | 0.955 |
| en | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| en | account | 34 | 1.000 | 0.676 | 0.807 | 1.000 | 0.676 | 0.807 |
| en | common_secret | 34 | 1.000 | 0.794 | 0.885 | 1.000 | 0.794 | 0.885 |
| en | private_date | 34 | 0.931 | 0.794 | 0.857 | 0.931 | 0.794 | 0.857 |
| en | **macro F1** | | | | **0.888** | | | **0.888** |
| mixed | private_person | 42 | 1.000 | 0.810 | 0.895 | 1.000 | 0.810 | 0.895 |
| mixed | private_address | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| mixed | account | 34 | 1.000 | 0.412 | 0.583 | 1.000 | 0.412 | 0.583 |
| mixed | common_secret | 34 | 1.000 | 0.882 | 0.938 | 1.000 | 0.882 | 0.938 |
| mixed | private_date | 36 | 1.000 | 0.861 | 0.925 | 1.000 | 0.861 | 0.925 |
| mixed | **macro F1** | | | | **0.856** | | | **0.856** |
| all | private_person | 139 | 1.000 | 0.856 | 0.922 | 1.000 | 0.856 | 0.922 |
| all | private_address | 104 | 1.000 | 0.885 | 0.939 | 1.000 | 0.885 | 0.939 |
| all | account | 105 | 1.000 | 0.600 | 0.750 | 1.000 | 0.600 | 0.750 |
| all | common_secret | 104 | 1.000 | 0.837 | 0.911 | 1.000 | 0.837 | 0.911 |
| all | private_date | 104 | 0.911 | 0.788 | 0.845 | 0.978 | 0.846 | 0.907 |
| all | **macro F1** | | | | **0.874** | | | **0.886** |

| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |
|---|---|---|---|---|
| zh | 1.2% (1/83) | 0.0% (0/83) | 0.00 (0 spans / 3303 tokens) | 83.0% (161/194) |
| en | 4.2% (4/95) | 0.0% (0/95) | 0.00 (0 spans / 12722 tokens) | 81.9% (149/182) |
| mixed | - | - | - | 77.2% (139/180) |
| all | 2.8% (5/178) | 0.0% (0/178) | 0.00 (0 spans / 16025 tokens) | 80.8% (449/556) |
| negative | 1.8% (3/166) | 0.0% (0/166) | 0.00 (0 spans / 8160 tokens) | - |
| request | 16.7% (2/12) | 0.0% (0/12) | 0.00 (0 spans / 7865 tokens) | - |

in-fence gold found: 0.0% (0/60); spans on clean units by label: {ip_address=5}; non-target labels in positives: {}

### examples scope=full threshold=0.80 (FP: label, score, 【span】; FN: gold, overlapping spans)

- FP | pos-private_person-zh-01 | private_date | 0.895 | 麻烦把报销单转给财务的汪海燕，她【今天下午】在。
- FP | pos-private_person-zh-13 | private_date | 0.877 | 麻烦把报销单转给财务的严佳琪，她【今天下午】在。
- FP | pos-private_person-zh-25 | private_date | 0.888 | 麻烦把报销单转给财务的龙春梅，她【今天下午】在。
- FP | pos-private_person-en-03 | private_date | 0.943 |  interned with us 【last summer】.
- FP | pos-private_person-en-15 | private_date | 0.941 |  interned with us 【last summer】.
- FP | pos-private_person-en-27 | private_date | 0.943 |  interned with us 【last summer】.
- FP | pos-private_person-mixed-06 | account | 0.811 | am 新来了一个 intern 叫 【Z】hiqiang Xie，帮我写个欢迎
- FP | pos-private_address-zh-02 | private_date | 0.905 | 我【下个月】搬到重庆市渝北区梧桐大道670号青松
- FP | pos-private_address-zh-14 | private_date | 0.887 | 我【下个月】搬到武汉市洪山区长青中路217号，帮
- FP | pos-private_address-zh-26 | private_date | 0.926 | 我【下个月】搬到厦门市思明区梧桐北路892号翠微
- FP | pos-private_address-mixed-07 | private_date | 0.941 | 0号长青华庭16栋6单元1806室 【next month】, 帮我做个 checklist。
- FP | pos-private_address-mixed-19 | private_date | 0.833 | to 广州市天河区望湖北路252号 【next month】, 帮我做个 checklist。
- FP | pos-account-en-25 | common_secret | 0.801 | Add me on WeChat: 【li8ciqvhcqr】.
- FP | pos-account-mixed-31 | common_secret | 0.885 | t password for 用户 【harbor_t5756】，他忘记了。
- FP | pos-common_secret-en-10 | account | 0.828 | :⏎```bash⏎curl -u 【admin】:xnwp-8meAts-RCDC-
- FP | pos-common_secret-en-10 | url | 0.997 | -RCDC-p63Jz-IMh3u 【https://staging.example.test/health】⏎```⏎and got 401.
- FP | pos-common_secret-en-22 | url | 0.996 | yEEB-v3pxtM-CgnHy 【https://staging.example.test/health】⏎```⏎and got 401.
- FP | pos-private_date-en-00 | private_date | 0.955 | how old will I be 【next year】?
- FP | pos-private_date-en-12 | private_date | 0.946 | how old will I be 【next year】?
- FP | pos-private_date-en-24 | private_date | 0.950 | how old will I be 【next year】?
- FP | neg-en-005 | ip_address | 0.994 | er listening addr=【127.0.0.1】:8080 mode=persona
- FP | neg-en-025 | private_person | 0.918 | Compare 【Hugo】, Gatsby and Astro
- FP | neg-en-048 | ip_address | 0.994 | nect ECONNREFUSED 【127.0.0.1】:8443⏎    at TCPCo
- FP | neg-zh-003 | ip_address | 0.993 | 47Z INFO  启动完成 监听 【127.0.0.1】:3000⏎2026-09-22T0
- FP | neg-zh-005 | private_person | 0.841 | 【李】代数和李群在机器人姿态估计里分别起什
- FP | neg-zh-035 | private_person | 0.876 | 【王】者荣耀的匹配机制是怎么设计的？从分布
- FP | neg-zh-035 | private_person | 0.853 | 王者【荣耀】的匹配机制是怎么设计的？从分布式系统
- FP | neg-zh-036 | private_person | 0.915 | 【江湖】上说 Rust 的所有权很难学，给我
- FP | neg-zh-048 | ip_address | 0.981 | nect ECONNREFUSED 【127.0.0.1】:9090⏎```⏎本地 Redis
- FP | req-002 | ip_address | 0.994 | er listening addr=【127.0.0.1】:5432 mode=persona
- FP | req-003 | ip_address | 0.994 | er listening addr=【127.0.0.1】:3000 mode=persona
- FP | req-005 | ip_address | 0.988 | nect ECONNREFUSED 【127.0.0.1】:3000⏎```⏎本地 Redis
- FP | req-015 | ip_address | 0.994 | er listening addr=【127.0.0.1】:7788 mode=persona
- FP | req-022 | ip_address | 0.994 | er listening addr=【127.0.0.1】:3000 mode=persona
- FN | pos-private_person-zh-07 | private_person | 帮我润色一下：尊敬的【诸葛敏】老师，您好，我是您去年带过的学生。 | private_person 0.60「诸葛敏老师」
- FN | pos-private_person-zh-19 | private_person | 帮我润色一下：尊敬的【任振华】老师，您好，我是您去年带过的学生。 | private_person 0.63「任振华老师」
- FN | pos-private_person-zh-31 | private_person | 帮我润色一下：尊敬的【范敏】老师，您好，我是您去年带过的学生。 | private_person 0.73「范敏老师」
- FN | pos-private_person-mixed-00 | private_person | 这个 PR 是 【Zheng Fang】 提的，帮我 review 一下 e | account 0.78「Z」; private_person 0.54「heng」; private_person 0.37「ang」
- FN | pos-private_person-mixed-06 | private_person | am 新来了一个 intern 叫 【Zhiqiang Xie】，帮我写个欢迎词。 | account 0.81「Z」; private_person 0.50「hiq」; account 0.67「i」; private_person 0.55「ang Xie」
- FN | pos-private_person-mixed-12 | private_person | 这个 PR 是 【Min Cao】 提的，帮我 review 一下 e | private_person 0.49「Cao」
- FN | pos-account-zh-04 | account | 会员卡号【4648 4937 5009】，积分怎么一直没到账？ | payment_card 0.50「4648 4937」; payment_card 0.53「5009」
- FN | pos-account-zh-28 | account | 会员卡号【9779 5461 4889】，积分怎么一直没到账？ | account 0.77「9779 5461 4889」
- FN | pos-account-zh-29 | account | 校园卡号【9821 4857 5881 8299】丢了，帮我写个挂失申请。 | account 0.78「9821 4857 5881 8299」
- FN | pos-account-zh-30 | private_person | 【高强】的微信号是wxid_oj8bnz31 | private_person 0.59「高强」
- FN | pos-account-en-01 | account | Add me on WeChat: 【wxid_kvi9fav8id8lcc】. | account 0.50「w」; common_secret 0.49「x」; account 0.48「id_kvi」; common_secret 0.48「9fav8id8lcc」
- FN | pos-account-en-03 | account | ty card number is 【4177 4670 4497】 and the points ar | account 0.79「4177 4670 4497」
- FN | pos-account-en-05 | account | lores's handle is 【paultiger14】; tag him in the r | common_secret 0.42「pa」; account 0.45「tiger14」
- FN | pos-account-en-15 | account | ty card number is 【5839 9746 4412】 and the points ar | account 0.80「5839 9746 4412」
- FN | pos-account-en-25 | account | Add me on WeChat: 【li8ciqvhcqr】. | common_secret 0.80「li8ciqvhcqr」
- FN | pos-account-mixed-01 | account | WeChat 加我 【j8rvyhrz3】，有 update 就发我。 | account 0.49「j8rvyhrz3」
- FN | pos-account-mixed-06 | account | 付冬雪 的 handle 是 【min.sun91】，在 Slack 里 @ 一下。 | 
- FN | pos-account-mixed-07 | account | t password for 用户 【meganharbor19】，他忘记了。 | account 0.68「meganharbor19」
- FN | pos-account-mixed-10 | account | `json⏎{"wechat": "【n10txd】"}⏎```⏎validate 一下 | 
- FN | pos-account-mixed-13 | account | WeChat 加我 【wxid_v04zolj5fq0rq9】，有 update 就发我。 | account 0.54「wxid_v04zolj5fq0rq9」
- FN | pos-account-mixed-14 | account | My 工号 is 【1497960】，HR 系统里查不到。 | account 0.75「1497960」
- FN | pos-account-mixed-16 | account | 会员 number 【9649 3206 4500 2249】 的 points 一直没更新。 | account 0.75「9649 3206 4500 2249」
- FN | pos-account-mixed-18 | account | 史平 的 handle 是 【matthew_copper7】，在 Slack 里 @ 一下。 | common_secret 0.43「matthew_copper7」
- FN | pos-account-mixed-19 | account | t password for 用户 【lucastiger90】，他忘记了。 | common_secret 0.59「lucastiger90」
- FN | pos-account-mixed-22 | account | `json⏎{"wechat": "【ku-4fj】"}⏎```⏎validate 一下 | common_secret 0.43「j」
- FN | pos-account-mixed-31 | account | t password for 用户 【harbor_t5756】，他忘记了。 | common_secret 0.89「harbor_t5756」
- FN | pos-common_secret-zh-01 | common_secret | 我把 【Pepper67@aw】 贴在这，你帮我测一下这个接口能不能 | common_secret 0.64「Pepper67@aw」
- FN | pos-common_secret-zh-32 | common_secret | 用 【Falcon330?QQ】 签名之后服务端一直返回 401，哪 | common_secret 0.56「Falcon330」
- FN | pos-common_secret-en-01 | common_secret | I pasted 【yn4ES0zrKpd4qBKH7mw2VbGp】 into the header a | 
- FN | pos-common_secret-en-16 | common_secret | The vendor sent 【Copper2775%UH】 over chat; is tha | common_secret 0.79「Copper2775%UH」
- FN | pos-private_date-mixed-27 | private_date | 郑嘉怡 的 DOB 是 【2013.04.05】，帮我填 form。 | 

### examples scope=skip_fenced threshold=0.80 (FP: label, score, 【span】; FN: gold, overlapping spans)

- FP | pos-private_person-zh-01 | private_date | 0.895 | 麻烦把报销单转给财务的汪海燕，她【今天下午】在。
- FP | pos-private_person-zh-13 | private_date | 0.877 | 麻烦把报销单转给财务的严佳琪，她【今天下午】在。
- FP | pos-private_person-zh-25 | private_date | 0.888 | 麻烦把报销单转给财务的龙春梅，她【今天下午】在。
- FP | pos-private_person-en-03 | private_date | 0.943 |  interned with us 【last summer】.
- FP | pos-private_person-en-15 | private_date | 0.941 |  interned with us 【last summer】.
- FP | pos-private_person-en-27 | private_date | 0.943 |  interned with us 【last summer】.
- FP | pos-private_person-mixed-06 | account | 0.811 | am 新来了一个 intern 叫 【Z】hiqiang Xie，帮我写个欢迎
- FP | pos-private_address-zh-02 | private_date | 0.905 | 我【下个月】搬到重庆市渝北区梧桐大道670号青松
- FP | pos-private_address-zh-14 | private_date | 0.887 | 我【下个月】搬到武汉市洪山区长青中路217号，帮
- FP | pos-private_address-zh-26 | private_date | 0.926 | 我【下个月】搬到厦门市思明区梧桐北路892号翠微
- FP | pos-private_address-mixed-07 | private_date | 0.941 | 0号长青华庭16栋6单元1806室 【next month】, 帮我做个 checklist。
- FP | pos-private_address-mixed-19 | private_date | 0.833 | to 广州市天河区望湖北路252号 【next month】, 帮我做个 checklist。
- FP | pos-account-en-25 | common_secret | 0.801 | Add me on WeChat: 【li8ciqvhcqr】.
- FP | pos-account-mixed-31 | common_secret | 0.885 | t password for 用户 【harbor_t5756】，他忘记了。
- FP | pos-private_date-en-00 | private_date | 0.955 | how old will I be 【next year】?
- FP | pos-private_date-en-12 | private_date | 0.946 | how old will I be 【next year】?
- FP | pos-private_date-en-24 | private_date | 0.950 | how old will I be 【next year】?
- FP | neg-en-000 | account | 0.804 | ng⏎}⏎```⏎Why does 【golangci-lint】 complain that ctx
- FP | neg-en-005 | ip_address | 0.994 | er listening addr=【127.0.0.1】:8080 mode=persona
- FP | neg-en-025 | private_person | 0.918 | Compare 【Hugo】, Gatsby and Astro
- FP | neg-en-048 | ip_address | 0.994 | nect ECONNREFUSED 【127.0.0.1】:8443⏎    at TCPCo
- FP | neg-zh-003 | ip_address | 0.993 | 47Z INFO  启动完成 监听 【127.0.0.1】:3000⏎2026-09-22T0
- FP | neg-zh-005 | private_person | 0.841 | 【李】代数和李群在机器人姿态估计里分别起什
- FP | neg-zh-035 | private_person | 0.876 | 【王】者荣耀的匹配机制是怎么设计的？从分布
- FP | neg-zh-035 | private_person | 0.853 | 王者【荣耀】的匹配机制是怎么设计的？从分布式系统
- FP | neg-zh-036 | private_person | 0.915 | 【江湖】上说 Rust 的所有权很难学，给我
- FP | req-002 | ip_address | 0.994 | er listening addr=【127.0.0.1】:5432 mode=persona
- FP | req-003 | ip_address | 0.994 | er listening addr=【127.0.0.1】:3000 mode=persona
- FP | req-015 | ip_address | 0.994 | er listening addr=【127.0.0.1】:7788 mode=persona
- FP | req-022 | ip_address | 0.994 | er listening addr=【127.0.0.1】:3000 mode=persona
- FN | pos-private_person-zh-07 | private_person | 帮我润色一下：尊敬的【诸葛敏】老师，您好，我是您去年带过的学生。 | private_person 0.60「诸葛敏老师」
- FN | pos-private_person-zh-10 | private_person | ```json⏎{"name": "【陶秋生】", "dept": "销售部"}⏎ | 
- FN | pos-private_person-zh-11 | private_person | 的表格：⏎```⏎姓名,部门,级别⏎【杨芳】,研发部,P6⏎```⏎统计一下每个 | 
- FN | pos-private_person-zh-19 | private_person | 帮我润色一下：尊敬的【任振华】老师，您好，我是您去年带过的学生。 | private_person 0.63「任振华老师」
- FN | pos-private_person-zh-22 | private_person | ```json⏎{"name": "【叶芳】", "dept": "销售部"}⏎ | 
- FN | pos-private_person-zh-23 | private_person | 的表格：⏎```⏎姓名,部门,级别⏎【诸葛冬雪】,研发部,P6⏎```⏎统计一下每个 | 
- FN | pos-private_person-zh-31 | private_person | 帮我润色一下：尊敬的【范敏】老师，您好，我是您去年带过的学生。 | private_person 0.73「范敏老师」
- FN | pos-private_person-en-10 | private_person | son⏎{"customer": "【Charles Johnson】", "tier": "gold"} | 
- FN | pos-private_person-en-11 | private_person | is:⏎```⏎name,team⏎【Lucas Johnson】,platform⏎```⏎Why  | 
- FN | pos-private_person-en-22 | private_person | son⏎{"customer": "【Ivy Adams】", "tier": "gold"} | 
- FN | pos-private_person-en-23 | private_person | is:⏎```⏎name,team⏎【Patricia Brown】,platform⏎```⏎Why  | 
- FN | pos-private_person-mixed-00 | private_person | 这个 PR 是 【Zheng Fang】 提的，帮我 review 一下 e | account 0.78「Z」; private_person 0.54「heng」; private_person 0.37「ang」
- FN | pos-private_person-mixed-06 | private_person | am 新来了一个 intern 叫 【Zhiqiang Xie】，帮我写个欢迎词。 | account 0.81「Z」; private_person 0.50「hiq」; account 0.67「i」; private_person 0.55「ang Xie」
- FN | pos-private_person-mixed-10 | private_person | 据：⏎```yaml⏎owner: 【吕建国】⏎team: infra⏎```⏎帮 | 
- FN | pos-private_person-mixed-11 | private_person | rs(name) VALUES ('【Lisa White】');⏎```⏎为什么插入失败？ | 
- FN | pos-private_person-mixed-12 | private_person | 这个 PR 是 【Min Cao】 提的，帮我 review 一下 e | private_person 0.49「Cao」
- FN | pos-private_person-mixed-22 | private_person | 据：⏎```yaml⏎owner: 【金欣怡】⏎team: infra⏎```⏎帮 | 
- FN | pos-private_person-mixed-23 | private_person | rs(name) VALUES ('【Mark Roberts】');⏎```⏎为什么插入失败？ | 
- FN | pos-private_address-zh-10 | private_address | json⏎{"address": "【上海市浦东新区银杏北路230号金桂家园7栋3单元3105室】"}⏎```⏎帮我校验地址字段。 | 
- FN | pos-private_address-zh-11 | private_address | 里这一行：⏎```⏎ship_to=【上海市徐汇区翠微街826号海棠小区29栋1单元1806室】 status=pending⏎`` | 
- FN | pos-private_address-zh-22 | private_address | json⏎{"address": "【厦门市思明区翠微大道916号星河花园12栋4单元2008室】"}⏎```⏎帮我校验地址字段。 | 
- FN | pos-private_address-zh-23 | private_address | 里这一行：⏎```⏎ship_to=【深圳市南山区枫林南路349号】 status=pending⏎`` | 
- FN | pos-private_address-en-10 | private_address | ipping_address": "【1873 Maple Street, Apt 40B, Salem, MA 62721】"}⏎```⏎Why does va | 
- FN | pos-private_address-en-11 | private_address | m the export:⏎```⏎【1782 Chestnut St, Madison, WI 10703】\|pending\|2⏎```⏎spl | 
- FN | pos-private_address-en-22 | private_address | ipping_address": "【601 Cedar Lane, Apt 34D, Brookfield, WI 13009】"}⏎```⏎Why does va | 
- FN | pos-private_address-en-23 | private_address | m the export:⏎```⏎【9226 Willow Street, Apt 12A, Riverton, WY 67816】\|pending\|2⏎```⏎spl | 
- FN | pos-private_address-mixed-10 | private_address | ```json⏎{"addr": "【深圳市南山区海棠中路397号梧桐名邸13栋4单元906室】"}⏎```⏎parse 一下省市区 | 
- FN | pos-private_address-mixed-11 | private_address | 有一行：⏎```⏎address: 【3482 Chestnut Avenue, Apt 24B, Georgetown, TX 64261】⏎```⏎帮我改成多行格式。 | 
- FN | pos-private_address-mixed-22 | private_address | ```json⏎{"addr": "【武汉市洪山区金桂西路540号】"}⏎```⏎parse 一下省市区 | 
- FN | pos-private_address-mixed-23 | private_address | 有一行：⏎```⏎address: 【7944 Sycamore Ave, Apt 33D, Milford, CT 81978】⏎```⏎帮我改成多行格式。 | 
- FN | pos-account-zh-04 | account | 会员卡号【4648 4937 5009】，积分怎么一直没到账？ | payment_card 0.50「4648 4937」; payment_card 0.53「5009」
- FN | pos-account-zh-10 | account | ：⏎```json⏎{"qq": "【3792967225】", "level": 3}⏎``` | 
- FN | pos-account-zh-11 | account | ml⏎owner_account: 【lunar_v2177】⏎```⏎这个字段在哪里用到了？ | 
- FN | pos-account-zh-22 | account | ：⏎```json⏎{"qq": "【1572606131】", "level": 3}⏎``` | 
- FN | pos-account-zh-23 | account | ml⏎owner_account: 【jiayi.lin87】⏎```⏎这个字段在哪里用到了？ | 
- FN | pos-account-zh-28 | account | 会员卡号【9779 5461 4889】，积分怎么一直没到账？ | account 0.77「9779 5461 4889」
- FN | pos-account-zh-29 | account | 校园卡号【9821 4857 5881 8299】丢了，帮我写个挂失申请。 | account 0.78「9821 4857 5881 8299」
- FN | pos-account-zh-30 | private_person | 【高强】的微信号是wxid_oj8bnz31 | private_person 0.59「高强」
- FN | pos-account-en-01 | account | Add me on WeChat: 【wxid_kvi9fav8id8lcc】. | account 0.50「w」; common_secret 0.49「x」; account 0.48「id_kvi」; common_secret 0.48「9fav8id8lcc」
- FN | pos-account-en-03 | account | ty card number is 【4177 4670 4497】 and the points ar | account 0.79「4177 4670 4497」
- FN | pos-account-en-05 | account | lores's handle is 【paultiger14】; tag him in the r | common_secret 0.42「pa」; account 0.45「tiger14」
- FN | pos-account-en-10 | account | ``json⏎{"login": "【yuxuan.lin81】", "active": true} | 
- FN | pos-account-en-11 | account | et:⏎```⏎member_no=【5990 3038 5994】⏎```⏎should this b | 
- FN | pos-account-en-15 | account | ty card number is 【5839 9746 4412】 and the points ar | account 0.80「5839 9746 4412」
- FN | pos-account-en-22 | account | ``json⏎{"login": "【david_tiger47】", "active": true} | 
- FN | pos-account-en-23 | account | et:⏎```⏎member_no=【8667 1949 9694 1392】⏎```⏎should this b | 
- FN | pos-account-en-25 | account | Add me on WeChat: 【li8ciqvhcqr】. | common_secret 0.80「li8ciqvhcqr」
- FN | pos-account-mixed-01 | account | WeChat 加我 【j8rvyhrz3】，有 update 就发我。 | account 0.49「j8rvyhrz3」
- FN | pos-account-mixed-06 | account | 付冬雪 的 handle 是 【min.sun91】，在 Slack 里 @ 一下。 | 
- FN | pos-account-mixed-07 | account | t password for 用户 【meganharbor19】，他忘记了。 | account 0.68「meganharbor19」
- FN | pos-account-mixed-10 | account | `json⏎{"wechat": "【n10txd】"}⏎```⏎validate 一下 | 
- FN | pos-account-mixed-11 | account | ```⏎login ok user=【falcon_u7857】⏎```⏎这个 user 为什么会重 | 
- FN | pos-account-mixed-13 | account | WeChat 加我 【wxid_v04zolj5fq0rq9】，有 update 就发我。 | account 0.54「wxid_v04zolj5fq0rq9」
- FN | pos-account-mixed-14 | account | My 工号 is 【1497960】，HR 系统里查不到。 | account 0.75「1497960」
- FN | pos-account-mixed-16 | account | 会员 number 【9649 3206 4500 2249】 的 points 一直没更新。 | account 0.75「9649 3206 4500 2249」
- FN | pos-account-mixed-18 | account | 史平 的 handle 是 【matthew_copper7】，在 Slack 里 @ 一下。 | common_secret 0.43「matthew_copper7」
- FN | pos-account-mixed-19 | account | t password for 用户 【lucastiger90】，他忘记了。 | common_secret 0.59「lucastiger90」
- FN | pos-account-mixed-22 | account | `json⏎{"wechat": "【ku-4fj】"}⏎```⏎validate 一下 | 
- FN | pos-account-mixed-23 | account | ```⏎login ok user=【jing.wu88】⏎```⏎这个 user 为什么会重 | 
- FN | pos-account-mixed-31 | account | t password for 用户 【harbor_t5756】，他忘记了。 | common_secret 0.89「harbor_t5756」
- FN | pos-common_secret-zh-01 | common_secret | 我把 【Pepper67@aw】 贴在这，你帮我测一下这个接口能不能 | common_secret 0.64「Pepper67@aw」
- FN | pos-common_secret-zh-10 | common_secret | deploy.sh --token 【Lunar2917@JrhC】⏎```⏎结果报权限错误。 | 
- FN | pos-common_secret-zh-11 | common_secret | 配置文件：⏎```⏎【f6l44jAJOcGtWX4bqrsiC67ZIb6NABA】⏎```⏎这一行是第三方给的，放哪里 | 
- FN | pos-common_secret-zh-22 | common_secret | deploy.sh --token 【aqOUpihPbvN12xTSnyKOH6XlJRH3j7GNa】⏎```⏎结果报权限错误。 | 
- FN | pos-common_secret-zh-23 | common_secret | 配置文件：⏎```⏎【opEftjlfoePIQASheZD3r9R4Wzxf6PHZ45jZnMQ】⏎```⏎这一行是第三方给的，放哪里 | 
- FN | pos-common_secret-zh-32 | common_secret | 用 【Falcon330?QQ】 签名之后服务端一直返回 401，哪 | common_secret 0.56「Falcon330」
- FN | pos-common_secret-en-01 | common_secret | I pasted 【yn4ES0zrKpd4qBKH7mw2VbGp】 into the header a | 
- FN | pos-common_secret-en-10 | common_secret | ash⏎curl -u admin:【xnwp-8meAts-RCDC-p63Jz-IMh3u】 https://staging.e | 
- FN | pos-common_secret-en-11 | common_secret | has one line:⏎```⏎【Pepper6101#EH】⏎```⏎where should  | 
- FN | pos-common_secret-en-16 | common_secret | The vendor sent 【Copper2775%UH】 over chat; is tha | common_secret 0.79「Copper2775%UH」
- FN | pos-common_secret-en-22 | common_secret | ash⏎curl -u admin:【dUKdE-yEEB-v3pxtM-CgnHy】 https://staging.e | 
- FN | pos-common_secret-en-23 | common_secret | has one line:⏎```⏎【GsNZz-8XI13-draHZ-b2WBL】⏎```⏎where should  | 
- FN | pos-common_secret-mixed-10 | common_secret | ⏎export APP_TOKEN=【UaxXL-YkG3-uuOm-H96M】⏎```⏎但是 app 读不到。 | 
- FN | pos-common_secret-mixed-11 | common_secret | secret 文件：⏎```⏎【aiKGSII3RwLvL5gHC3wTaFBE3+ZrdFP7/8/HcTv8yNKL】⏎```⏎应该放在哪个目录？ | 
- FN | pos-common_secret-mixed-22 | common_secret | ⏎export APP_TOKEN=【PfF1N-kIHi-qDjF-V8Ea1d】⏎```⏎但是 app 读不到。 | 
- FN | pos-common_secret-mixed-23 | common_secret | secret 文件：⏎```⏎【fJremHpdkODbaVF60lTxKsNcA0UiZQX73cp】⏎```⏎应该放在哪个目录？ | 
- FN | pos-private_date-zh-10 | private_date | son⏎{"birthday": "【1978年3月1日】"}⏎```⏎帮我算年龄。 | 
- FN | pos-private_date-zh-11 | private_date | 患者记录：⏎```⏎预约日期: 【2019年5月8日】⏎```⏎帮我生成提醒短信。 | 
- FN | pos-private_date-zh-22 | private_date | son⏎{"birthday": "【1971年11月26号】"}⏎```⏎帮我算年龄。 | 
- FN | pos-private_date-zh-23 | private_date | 患者记录：⏎```⏎预约日期: 【1983/4/4】⏎```⏎帮我生成提醒短信。 | 
- FN | pos-private_date-en-10 | private_date | ⏎```json⏎{"dob": "【2011-04-19】"}⏎```⏎compute the | 
- FN | pos-private_date-en-11 | private_date | ⏎```⏎appointment: 【2006-11-24】⏎```⏎format it for | 
- FN | pos-private_date-en-22 | private_date | ⏎```json⏎{"dob": "【16 August 2009】"}⏎```⏎compute the | 
- FN | pos-private_date-en-23 | private_date | ⏎```⏎appointment: 【1 May 2013】⏎```⏎format it for | 
- FN | pos-private_date-mixed-10 | private_date | ⏎```json⏎{"dob": "【2011-02-08】"}⏎```⏎算一下 age。 | 
- FN | pos-private_date-mixed-11 | private_date | ：⏎```⏎birth_date: 【1983.11.20】⏎```⏎转成 ISO format | 
- FN | pos-private_date-mixed-22 | private_date | ⏎```json⏎{"dob": "【10 July 1971】"}⏎```⏎算一下 age。 | 
- FN | pos-private_date-mixed-23 | private_date | ：⏎```⏎birth_date: 【2004年5月11日】⏎```⏎转成 ISO format | 
- FN | pos-private_date-mixed-27 | private_date | 郑嘉怡 的 DOB 是 【2013.04.05】，帮我填 form。 | 
