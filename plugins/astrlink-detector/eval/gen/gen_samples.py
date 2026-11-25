#!/usr/bin/env python3
"""Generate the NER evaluation set for AstrLink Guard (bench/ner/data/*.jsonl).

Every value is synthetic: names are random surname + given-name pairs,
addresses use made-up streets, numbers and keys are random. Nothing here is
real personal data. The output is deterministic (fixed seed), so re-running
the script reproduces the committed files byte for byte.

Each line is one sample:

  {"id", "group": positive|negative|request, "lang": zh|en|mixed,
   "category": <primary label or "clean">, "in_fence": bool,
   "text": str, "entities": [{"label", "start", "end", "text"}]}

Offsets are Unicode code point offsets into `text`, `end` exclusive (Python
string indices). Labels use AstrLink's names (private_person, private_address,
account, common_secret, private_date, plus email/phone/url/ip_address when a
template needs them).

Request samples (group=request) carry "segments": a list of
{"role", "text", "entities"} that mimic how the gateway calls a detector once
per message string.

Usage: python3 gen_samples.py <out_dir>
"""
import json
import os
import random
import string
import sys

R = random.Random(20261125)

# ---------------------------------------------------------------- pools

ZH_SURNAMES = list("王李张刘陈杨黄赵吴周徐孙马朱胡郭何高林罗郑梁谢宋唐许韩冯邓曹彭曾田董潘袁蔡蒋余于杜叶程魏苏吕丁任卢姚沈钟姜崔谭陆范汪廖石金韦贾夏付方邹熊白孟秦邱侯江尹薛段雷龙黎史陶贺毛郝顾龚邵万武钱戴严") + ["欧阳", "司马", "上官", "诸葛"]
ZH_GIVEN = ["伟", "芳", "娜", "敏", "静", "丽", "强", "磊", "军", "洋", "勇", "艳", "杰", "娟", "涛", "明", "超", "秀英", "霞", "平", "刚", "桂英", "晓东", "子涵", "浩然", "一诺", "雨桐", "梓萱", "思远", "嘉怡", "俊杰", "欣怡", "宇轩", "婉清", "志强", "海燕", "建国", "文博", "晨曦", "若溪", "天佑", "佳琪", "明哲", "雅婷", "振华", "立新", "春梅", "秋生", "冬雪", "小芸", "德胜", "淑珍", "鹏飞", "慧敏"]
EN_FIRST = ["James", "Mary", "Robert", "Patricia", "John", "Jennifer", "Michael", "Linda", "David", "Elizabeth", "William", "Barbara", "Richard", "Susan", "Joseph", "Jessica", "Thomas", "Sarah", "Charles", "Karen", "Daniel", "Nancy", "Matthew", "Lisa", "Anthony", "Betty", "Mark", "Sandra", "Steven", "Ashley", "Paul", "Emily", "Andrew", "Megan", "Kevin", "Hannah", "Brian", "Olivia", "Ethan", "Chloe", "Liam", "Nora", "Owen", "Grace", "Lucas", "Ivy", "Marcus", "Priya", "Tomasz", "Ingrid"]
EN_LAST = ["Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Taylor", "Moore", "Jackson", "Martin", "Perez", "Thompson", "White", "Harris", "Sanchez", "Clark", "Ramirez", "Lewis", "Robinson", "Walker", "Young", "Allen", "King", "Wright", "Scott", "Torres", "Nguyen", "Hill", "Flores", "Green", "Adams", "Nelson", "Baker", "Hall", "Rivera", "Campbell", "Mitchell", "Carter", "Roberts", "Whitfield", "Okafor", "Lindqvist", "Kowalski", "Brennan", "Haddad"]
PINYIN_SUR = ["Zhang", "Wang", "Li", "Liu", "Chen", "Yang", "Huang", "Zhao", "Zhou", "Wu", "Xu", "Sun", "Ma", "Zhu", "Hu", "Guo", "Lin", "Luo", "Zheng", "Liang", "Xie", "Tang", "Han", "Feng", "Deng", "Cao", "Peng", "Zeng", "Pan", "Jiang"]
PINYIN_GIVEN = ["Wei", "Fang", "Na", "Min", "Jing", "Li", "Qiang", "Lei", "Jun", "Yang", "Yong", "Yan", "Jie", "Juan", "Tao", "Ming", "Chao", "Xiaodong", "Zihan", "Haoran", "Yinuo", "Yutong", "Siyuan", "Jiayi", "Junjie", "Xinyi", "Yuxuan", "Wanqing", "Zhiqiang", "Haiyan"]

ZH_CITIES = ["北京市海淀区", "北京市朝阳区", "上海市浦东新区", "上海市徐汇区", "广州市天河区", "深圳市南山区", "杭州市西湖区", "成都市武侯区", "武汉市洪山区", "南京市鼓楼区", "西安市雁塔区", "苏州市工业园区", "重庆市渝北区", "天津市南开区", "长沙市岳麓区", "厦门市思明区"]
ZH_ROAD_A = ["长青", "文澜", "望湖", "金桂", "银杏", "云栖", "青松", "白鹭", "星河", "翠微", "梧桐", "海棠", "临江", "听雨", "枫林", "明月"]
ZH_ROAD_B = ["东路", "西路", "南路", "北路", "中路", "路", "街", "大道"]
ZH_ESTATE = ["花园", "家园", "雅苑", "小区", "公寓", "新村", "华庭", "名邸"]
EN_STREETS = ["Maple", "Oak", "Cedar", "Willow", "Birch", "Hawthorne", "Juniper", "Linden", "Sycamore", "Aspen", "Chestnut", "Magnolia", "Briar", "Foxglove"]
EN_SUFFIX = ["St", "Street", "Ave", "Avenue", "Road", "Lane", "Drive", "Court", "Blvd"]
EN_CITIES = [("Springfield", "IL"), ("Riverton", "WY"), ("Fairview", "OR"), ("Lakewood", "CO"), ("Brookfield", "WI"), ("Ashland", "OR"), ("Georgetown", "TX"), ("Salem", "MA"), ("Madison", "WI"), ("Oakdale", "CA"), ("Milford", "CT"), ("Greenville", "SC")]
EN_MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"]
EN_MON3 = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
WORDS = ["river", "copper", "falcon", "maple", "orbit", "pepper", "sunset", "violet", "harbor", "tiger", "lunar", "cobalt"]


def zh_name():
    return R.choice(ZH_SURNAMES) + R.choice(ZH_GIVEN)


def en_name(full=True):
    return R.choice(EN_FIRST) + (" " + R.choice(EN_LAST) if full else "")


def pinyin_name():
    if R.random() < 0.5:
        return R.choice(PINYIN_SUR) + " " + R.choice(PINYIN_GIVEN)
    return R.choice(PINYIN_GIVEN) + " " + R.choice(PINYIN_SUR)


def zh_addr():
    a = R.choice(ZH_CITIES) + R.choice(ZH_ROAD_A) + R.choice(ZH_ROAD_B) + f"{R.randint(1, 999)}号"
    if R.random() < 0.6:
        a += R.choice(ZH_ROAD_A) + R.choice(ZH_ESTATE) + f"{R.randint(1, 30)}栋{R.randint(1, 6)}单元{R.randint(1, 32)}0{R.randint(1, 8)}室"
    return a


def en_addr():
    city, st = R.choice(EN_CITIES)
    apt = f"Apt {R.randint(1, 40)}{R.choice('ABCD')}, " if R.random() < 0.4 else ""
    return f"{R.randint(10, 9999)} {R.choice(EN_STREETS)} {R.choice(EN_SUFFIX)}, {apt}{city}, {st} {R.randint(10000, 99999)}"


def b62(n):
    return "".join(R.choice(string.ascii_letters + string.digits) for _ in range(n))


def secret():
    k = R.random()
    if k < 0.35:
        return b62(R.randint(24, 40))
    if k < 0.55:
        return "-".join(b62(R.choice([4, 5, 6])) for _ in range(R.randint(4, 5)))
    if k < 0.75:
        s = "".join(R.choice(string.ascii_letters + string.digits + "+/") for _ in range(R.choice([32, 40, 44])))
        return s + ("=" if R.random() < 0.5 else "")
    # password-like
    w = R.choice(WORDS)
    return w[0].upper() + w[1:] + str(R.randint(10, 9999)) + R.choice("!@#$%&*?") + b62(R.randint(2, 5))


def qq():
    return str(R.randint(10000, 3999999999))


def wechat():
    if R.random() < 0.4:
        return "wxid_" + "".join(R.choice(string.ascii_lowercase + string.digits) for _ in range(14))
    return R.choice(string.ascii_lowercase) + "".join(R.choice(string.ascii_lowercase + string.digits + "_-") for _ in range(R.randint(5, 12)))


def username():
    w = R.choice(WORDS)
    pat = R.random()
    if pat < 0.3:
        return f"{w}_{R.choice(string.ascii_lowercase)}{R.randint(1, 9999)}"
    if pat < 0.6:
        return f"{R.choice(PINYIN_GIVEN).lower()}.{R.choice(PINYIN_SUR).lower()}{R.randint(80, 99)}"
    return f"{R.choice(EN_FIRST).lower()}{R.choice(['_', '', '.'])}{R.choice(WORDS)}{R.randint(1, 99)}"


def employee_id():
    return R.choice(["E", "EMP", "HR", "A", ""]) + str(R.randint(100000, 9999999))


def member_no():
    return " ".join(str(R.randint(1000, 9999)) for _ in range(R.choice([3, 4])))


def account():
    return R.choice([qq, wechat, username, employee_id, member_no])()


def zh_date():
    y, m, d = R.randint(1960, 2032), R.randint(1, 12), R.randint(1, 28)
    return R.choice([f"{y}年{m}月{d}日", f"{y}-{m:02d}-{d:02d}", f"{y}/{m}/{d}", f"{y}年{m}月{d}号", f"{y}.{m:02d}.{d:02d}"])


def en_date():
    y, m, d = R.randint(1960, 2032), R.randint(1, 12), R.randint(1, 28)
    return R.choice([f"{EN_MONTHS[m-1]} {d}, {y}", f"{d} {EN_MONTHS[m-1]} {y}", f"{m:02d}/{d:02d}/{y}", f"{y}-{m:02d}-{d:02d}", f"{EN_MON3[m-1]} {d}, {y}"])


def zh_md():
    return f"{R.randint(1, 12)}月{R.randint(1, 28)}日"


def phone_cn():
    return "1" + R.choice("3456789") + "".join(R.choice(string.digits) for _ in range(9))


# Slot fillers: name -> (label, generator)
SLOTS = {
    "ZPER": ("private_person", zh_name),
    "EPER": ("private_person", en_name),
    "EFIRST": ("private_person", lambda: en_name(False)),
    "PPER": ("private_person", pinyin_name),
    "ZADDR": ("private_address", zh_addr),
    "EADDR": ("private_address", en_addr),
    "ACCT": ("account", account),
    "QQ": ("account", qq),
    "WX": ("account", wechat),
    "USER": ("account", username),
    "EMPID": ("account", employee_id),
    "MEMBER": ("account", member_no),
    "SECRET": ("common_secret", secret),
    "ZDATE": ("private_date", zh_date),
    "EDATE": ("private_date", en_date),
    "ZMD": ("private_date", zh_md),
    "PHONE": ("phone", phone_cn),
}


def fill(tmpl):
    """Expand {SLOT} placeholders, returning (text, entities)."""
    out, ents, i = [], [], 0
    pos = 0
    while i < len(tmpl):
        if tmpl[i:i + 2] in ("{{", "}}"):  # escaped brace, as in str.format
            out.append(tmpl[i])
            pos += 1
            i += 2
            continue
        if tmpl[i] == "{" and "}" in tmpl[i:]:
            j = tmpl.index("}", i)
            key = tmpl[i + 1:j]
            if key in SLOTS:
                label, gen = SLOTS[key]
                v = gen()
                ents.append({"label": label, "start": pos, "end": pos + len(v), "text": v})
                out.append(v)
                pos += len(v)
                i = j + 1
                continue
        out.append(tmpl[i])
        pos += 1
        i += 1
    return "".join(out), ents


# ---------------------------------------------------------------- positives
# Templates per (category, lang). A template whose text holds ``` puts the
# entity inside a fenced block (in_fence=True).

POS = {
    ("private_person", "zh"): [
        "这个需求是{ZPER}提的，你先看看能不能在周五前做完。",
        "麻烦把报销单转给财务的{ZPER}，她今天下午在。",
        "昨天开会{ZPER}说接口要改成分页返回，你同意吗？",
        "请帮我给{ZPER}写一封感谢信，感谢他在项目上线时的支持。",
        "我妈叫{ZPER}，帮我在表格里把联系人改成她。",
        "{ZPER}和{ZPER}是这次答辩的评委，帮我准备一份介绍。",
        "客户那边的对接人换成了{ZPER}，之前的人离职了。",
        "帮我润色一下：尊敬的{ZPER}老师，您好，我是您去年带过的学生。",
        "这份合同的乙方签字人是{ZPER}，帮我检查条款有没有问题。",
        "我同事{ZPER}说这个 bug 是缓存没清导致的，你怎么看？",
        "测试数据如下：\n```json\n{{\"name\": \"{ZPER}\", \"dept\": \"销售部\"}}\n```\n帮我写个解析函数。",
        "下面是导出的表格：\n```\n姓名,部门,级别\n{ZPER},研发部,P6\n```\n统计一下每个部门的人数。",
    ],
    ("private_person", "en"): [
        "Can you draft a reply to {EPER} saying we will ship the fix on Monday?",
        "My manager {EPER} asked me to summarize the incident for the board.",
        "Please add {EPER} as a reviewer, she knows the billing code best.",
        "I'm writing a recommendation letter for {EPER}, who interned with us last summer.",
        "{EFIRST} from the design team says the button color is off.",
        "The tenant on file is {EPER}; update the lease template accordingly.",
        "Forward the meeting notes to {EPER} and {EPER} before noon.",
        "Our landlord, {EPER}, wants the payment schedule in writing.",
        "Help me write a toast for my brother {EPER}'s wedding.",
        "According to {EPER}, the migration script failed on the second run.",
        "Here is the fixture:\n```json\n{{\"customer\": \"{EPER}\", \"tier\": \"gold\"}}\n```\nWrite a test that loads it.",
        "The CSV export looks like this:\n```\nname,team\n{EPER},platform\n```\nWhy does the parser drop the header?",
    ],
    ("private_person", "mixed"): [
        "这个 PR 是 {PPER} 提的，帮我 review 一下 error handling。",
        "Please ask {ZPER} to check the 部署脚本 before release.",
        "PM 是 {EPER}，她说 deadline 改到下周三了。",
        "帮我给 {EPER} 回个 email，说 contract 我们已经签好了。",
        "{ZPER} 的 onboarding 文档还没写完，can you help?",
        "I need to 跟 {ZPER} 确认一下 budget，帮我起草一条消息。",
        "我们 team 新来了一个 intern 叫 {PPER}，帮我写个欢迎词。",
        "The 客户 contact is {ZPER}, 记得在 CRM 里更新。",
        "{EPER} 说 staging 环境又挂了，帮我看下 log。",
        "帮我翻译成英文：{ZPER}负责本季度的 roadmap 规划。",
        "mock 数据：\n```yaml\nowner: {ZPER}\nteam: infra\n```\n帮我写个 loader。",
        "这是 seed 文件：\n```sql\nINSERT INTO users(name) VALUES ('{EPER}');\n```\n为什么插入失败？",
    ],
    ("private_address", "zh"): [
        "我的收货地址是{ZADDR}，麻烦帮我核对一下格式。",
        "快递一直没到，地址写的是{ZADDR}，帮我写个投诉。",
        "我下个月搬到{ZADDR}，帮我列个搬家清单。",
        "请把发票寄到{ZADDR}，收件人写我本人。",
        "租房合同里的房屋地址是{ZADDR}，帮我看看条款。",
        "我爸住在{ZADDR}，离最近的三甲医院有多远？",
        "{ZPER}的住址是{ZADDR}，帮我写一封挂号信的信封格式。",
        "公司搬家了，新办公地址：{ZADDR}，帮我更新邮件签名。",
        "帮我查下从{ZADDR}到机场怎么走最快。",
        "户口所在地填{ZADDR}，这样填对吗？",
        "订单数据：\n```json\n{{\"address\": \"{ZADDR}\"}}\n```\n帮我校验地址字段。",
        "日志里这一行：\n```\nship_to={ZADDR} status=pending\n```\n为什么一直 pending？",
    ],
    ("private_address", "en"): [
        "Please ship the replacement to {EADDR}.",
        "I just moved to {EADDR}; how do I update my voter registration?",
        "My mailing address is {EADDR}, can you format it for an envelope?",
        "Draft a noise complaint for my neighbor at {EADDR}.",
        "The property at {EADDR} needs a home inspection checklist.",
        "{EPER} lives at {EADDR}; write a short invitation card for her.",
        "Our new office is {EADDR}, update the footer of the website.",
        "How long is the drive from {EADDR} to the nearest airport?",
        "Return label should say {EADDR}.",
        "I need to change my billing address to {EADDR}.",
        "Fixture:\n```json\n{{\"shipping_address\": \"{EADDR}\"}}\n```\nWhy does validation reject it?",
        "The row from the export:\n```\n{EADDR}|pending|2\n```\nsplit it into columns for me.",
    ],
    ("private_address", "mixed"): [
        "帮我把这个 address 翻译成英文：{ZADDR}",
        "My parents live at {ZADDR}, 怎么用英文写信封？",
        "Ship to {EADDR}，帮我确认下 zip code 对不对。",
        "我的 billing address 是 {EADDR}，这个格式 OK 吗？",
        "新 office 在 {ZADDR}，帮我写个 announcement。",
        "请帮我 check 一下这个地址 {EADDR} 是不是在 downtown。",
        "Delivery 失败了，地址是 {ZADDR}，帮我联系 courier。",
        "I'm moving to {ZADDR} next month, 帮我做个 checklist。",
        "收件地址 {EADDR}，please print a label。",
        "这个 Airbnb 的地址是 {EADDR}，离 conference 会场远吗？",
        "seed data：\n```json\n{{\"addr\": \"{ZADDR}\"}}\n```\nparse 一下省市区。",
        "fixture 里有一行：\n```\naddress: {EADDR}\n```\n帮我改成多行格式。",
    ],
    ("account", "zh"): [
        "我的QQ是{QQ}，有问题可以随时加我。",
        "加我微信：{WX}，备注项目名。",
        "我在论坛上的用户名是{USER}，帮我看看为什么登录不了。",
        "我的工号是{EMPID}，请帮我查一下年假余额。",
        "会员卡号{MEMBER}，积分怎么一直没到账？",
        "校园卡号{MEMBER}丢了，帮我写个挂失申请。",
        "{ZPER}的微信号是{WX}，帮我把他拉进群。",
        "我游戏账号{USER}被盗了，帮我写申诉材料。",
        "这是我的员工编号 {EMPID}，系统里显示的部门不对。",
        "麻烦把权限开给账号{USER}，我是新来的。",
        "用户表里有一条：\n```json\n{{\"qq\": \"{QQ}\", \"level\": 3}}\n```\n帮我写个查询。",
        "配置片段：\n```yaml\nowner_account: {USER}\n```\n这个字段在哪里用到了？",
    ],
    ("account", "en"): [
        "My username on the forum is {USER}, can you reset it?",
        "Add me on WeChat: {WX}.",
        "Employee ID {EMPID} — please update my cost center.",
        "My loyalty card number is {MEMBER} and the points are missing.",
        "Log in as {USER} on the shared laptop and check the settings.",
        "{EPER}'s handle is {USER}; tag him in the release notes.",
        "Please grant access to account {USER} for the analytics dashboard.",
        "My membership number {MEMBER} expired, how do I renew?",
        "My QQ number is {QQ} if the team in Shenzhen needs it.",
        "Use my staff number {EMPID} for the badge request.",
        "Row from the users table:\n```json\n{{\"login\": \"{USER}\", \"active\": true}}\n```\nWhy is active ignored?",
        "Snippet:\n```\nmember_no={MEMBER}\n```\nshould this be normalized?",
    ],
    ("account", "mixed"): [
        "我的 GitHub username 是 {USER}，帮我加到 repo 里。",
        "WeChat 加我 {WX}，有 update 就发我。",
        "My 工号 is {EMPID}，HR 系统里查不到。",
        "QQ {QQ} 是我的备用联系方式，please note。",
        "会员 number {MEMBER} 的 points 一直没更新。",
        "帮我把 account {USER} 的 role 改成 admin。",
        "{ZPER} 的 handle 是 {USER}，在 Slack 里 @ 一下。",
        "Please reset password for 用户 {USER}，他忘记了。",
        "我在 Steam 上叫 {USER}，加个好友一起玩。",
        "employee ID {EMPID} 的 laptop 要回收。",
        "数据：\n```json\n{{\"wechat\": \"{WX}\"}}\n```\nvalidate 一下格式。",
        "log 里：\n```\nlogin ok user={USER}\n```\n这个 user 为什么会重复登录？",
    ],
    ("common_secret", "zh"): [
        "这是我刚生成的密钥：{SECRET}，帮我看看怎么配置到环境变量里。",
        "我把 {SECRET} 贴在这，你帮我测一下这个接口能不能通。",
        "管理员密码我改成了{SECRET}，帮我写个通知让大家别外传。",
        "调用的时候带上 {SECRET} 就行，你写个 curl 示例。",
        "这串 {SECRET} 是测试环境用的，帮我判断一下是不是泄露了。",
        "我数据库的密码是{SECRET}，为什么连不上？",
        "他发给我一个令牌 {SECRET}，说一个小时后过期。",
        "WiFi 密码：{SECRET}，帮我生成一张二维码说明卡。",
        "用 {SECRET} 签名之后服务端一直返回 401，哪里错了？",
        "{SECRET} 这个是我 VPN 的口令，能帮我设个自动登录吗？",
        "我在终端里执行了：\n```bash\n./deploy.sh --token {SECRET}\n```\n结果报权限错误。",
        "配置文件：\n```\n{SECRET}\n```\n这一行是第三方给的，放哪里合适？",
    ],
    ("common_secret", "en"): [
        "Here is the key I just generated: {SECRET}. How do I load it from env?",
        "I pasted {SECRET} into the header and still get 403.",
        "My database password is {SECRET} and the connection keeps timing out.",
        "Use {SECRET} when you call the staging endpoint.",
        "The vendor sent {SECRET} over chat; is that safe to commit?",
        "Can you check whether {SECRET} looks like a valid HMAC secret?",
        "The wifi passphrase is {SECRET}, make a guest card for the office.",
        "I rotated it, the new one is {SECRET}.",
        "Signing with {SECRET} gives a different digest than the server, why?",
        "Our shared admin password is {SECRET}, write a policy reminder.",
        "I ran:\n```bash\ncurl -u admin:{SECRET} https://staging.example.test/health\n```\nand got 401.",
        "The secrets file has one line:\n```\n{SECRET}\n```\nwhere should it live?",
    ],
    ("common_secret", "mixed"): [
        "这是新的 key：{SECRET}，please 帮我放进 vault。",
        "我用 {SECRET} 调 API 一直 401，怎么 debug？",
        "Admin 密码是 {SECRET}，记得 rotate。",
        "staging 的 token 是 {SECRET}，帮我写个 curl。",
        "{SECRET} 这个 secret 是从 vendor 那拿的，可以 commit 吗？",
        "WiFi password 改成 {SECRET} 了，帮我通知一下 team。",
        "用 {SECRET} sign 完的 request 被拒了。",
        "I got {SECRET} from 运维，是不是已经过期了？",
        "这个 passphrase {SECRET} 太短了吗？",
        "VPN 口令 {SECRET}，how to set auto login?",
        "我跑了：\n```bash\nexport APP_TOKEN={SECRET}\n```\n但是 app 读不到。",
        "secret 文件：\n```\n{SECRET}\n```\n应该放在哪个目录？",
    ],
    ("private_date", "zh"): [
        "我的生日是{ZDATE}，帮我算一下今年多大。",
        "我{ZMD}的航班飞成都，帮我列个行李清单。",
        "我的护照有效期到{ZDATE}，还需要换吗？",
        "身份证签发日期是{ZDATE}，有效期十年，什么时候到期？",
        "我约了{ZMD}下午三点的牙医，帮我设个提醒。",
        "{ZPER}出生于{ZDATE}，帮我写一份简历的个人信息部分。",
        "我{ZDATE}入职的，到现在满几年了？",
        "体检预约在{ZDATE}上午，需要空腹吗？",
        "我女儿的预产期是{ZDATE}，帮我做个待产清单。",
        "我的签证{ZDATE}到期，续签要准备什么？",
        "简历数据：\n```json\n{{\"birthday\": \"{ZDATE}\"}}\n```\n帮我算年龄。",
        "患者记录：\n```\n预约日期: {ZDATE}\n```\n帮我生成提醒短信。",
    ],
    ("private_date", "en"): [
        "I was born on {EDATE}; how old will I be next year?",
        "My flight to Denver is on {EDATE}, help me pack.",
        "My passport expires {EDATE}. Do I need to renew before the trip?",
        "I have a dentist appointment on {EDATE} at 3pm, set a reminder.",
        "{EPER} was born {EDATE}; draft the bio for the program.",
        "My driver's license was issued {EDATE}.",
        "Our baby is due {EDATE}, make a checklist.",
        "My visa runs out on {EDATE}, what do I need to extend it?",
        "I started this job on {EDATE}; when am I eligible for sabbatical?",
        "My MRI is scheduled for {EDATE}, can I eat before?",
        "Patient record:\n```json\n{{\"dob\": \"{EDATE}\"}}\n```\ncompute the age.",
        "The form row:\n```\nappointment: {EDATE}\n```\nformat it for an SMS.",
    ],
    ("private_date", "mixed"): [
        "我的 birthday 是 {ZDATE}，帮我 plan 个 party。",
        "My flight 是 {EDATE}，帮我查下 visa 要求。",
        "护照 expire date 是 {EDATE}，还能出国吗？",
        "{ZPER} 的 DOB 是 {ZDATE}，帮我填 form。",
        "我约了 {EDATE} 的 dentist，记得提醒我。",
        "Onboarding date 是 {ZDATE}，帮我算 probation 到哪天。",
        "我老婆的 due date 是 {EDATE}，帮我做个 checklist。",
        "Visa 在 {ZDATE} 到期，续签流程是什么？",
        "我 {ZDATE} 出生的，今年 how old？",
        "体检 appointment 在 {EDATE}，需要 fasting 吗？",
        "记录：\n```json\n{{\"dob\": \"{EDATE}\"}}\n```\n算一下 age。",
        "form 数据：\n```\nbirth_date: {ZDATE}\n```\n转成 ISO format。",
    ],
}

# ---------------------------------------------------------------- negatives
# Entity-free coding-assistant content. Placeholders {SHA}, {UUID}, {N},
# {TS}, {VER}, {PORT}, {HEX64}, {DUR} are filled with non-PII values (hashes,
# ids, timestamps that are not about a person).


def sha():
    return "".join(R.choice("0123456789abcdef") for _ in range(40))


def uuid():
    h = "".join(R.choice("0123456789abcdef") for _ in range(32))
    return f"{h[:8]}-{h[8:12]}-4{h[13:16]}-a{h[17:20]}-{h[20:]}"


def ts():
    return f"2026-{R.randint(1, 11):02d}-{R.randint(1, 28):02d}T{R.randint(0, 23):02d}:{R.randint(0, 59):02d}:{R.randint(0, 59):02d}Z"


NEG_FILL = {
    "SHA": sha, "SHORT": lambda: sha()[:7], "UUID": uuid, "N": lambda: str(R.randint(2, 900)),
    "TS": ts, "VER": lambda: f"v{R.randint(0, 3)}.{R.randint(0, 30)}.{R.randint(0, 9)}",
    "PORT": lambda: str(R.choice([3000, 5432, 6379, 8080, 8443, 9090, 7788])),
    "HEX64": lambda: "".join(R.choice("0123456789abcdef") for _ in range(64)),
    "DUR": lambda: f"{R.randint(1, 900) / 100:.2f}s", "MS": lambda: str(R.randint(1, 999)),
}


def neg_fill(t):
    for k, f in NEG_FILL.items():
        while "{" + k + "}" in t:
            t = t.replace("{" + k + "}", f(), 1)
    return t


NEG_EN = [
    "```go\npackage redact\n\nimport (\n\t\"context\"\n\t\"strings\"\n\n\t\"github.com/yiyiSmallti/precinct-gatehouse/internal/config\"\n)\n\n// Engine holds the configured detectors.\ntype Engine struct {\n\tcfg   config.Redaction\n\tterms []string\n}\n```\nWhy does golangci-lint complain that ctx is unused here?",
    "```ts\nexport class PersonNameFormatter {\n  constructor(private readonly locale: string) {}\n  format(first: string, last: string): string {\n    return this.locale === 'zh' ? `${last}${first}` : `${first} ${last}`;\n  }\n}\n```\nAdd a unit test for the zh branch.",
    "```python\nfrom dataclasses import dataclass\nfrom typing import Optional\n\n@dataclass\nclass UserProfile:\n    user_id: int\n    display_name: Optional[str] = None\n    email_verified: bool = False\n\n    def is_complete(self) -> bool:\n        return self.display_name is not None and self.email_verified\n```\nMake this a pydantic model instead.",
    "Running `go test ./...` gives:\n```\n--- FAIL: TestVaultRestore ({DUR})\n    vault_test.go:{N}: got \"{{PERSON_a1b2c3d4}}\", want \"restored\"\nFAIL\nFAIL\tgithub.com/yiyiSmallti/precinct-gatehouse/internal/redact\t{DUR}\nok  \tgithub.com/yiyiSmallti/precinct-gatehouse/internal/config\t{DUR}\n```\nWhat is wrong with the restore path?",
    "npm ERR! code ERESOLVE\nnpm ERR! ERESOLVE unable to resolve dependency tree\nnpm ERR! Found: react@18.3.1\nnpm ERR! Could not resolve dependency:\nnpm ERR! peer react@\"^17.0.0\" from react-beautiful-dnd@13.1.1\nnpm ERR! Fix the upstream dependency conflict, or retry with --legacy-peer-deps",
    "{TS} INFO  server listening addr=127.0.0.1:{PORT} mode=personal\n{TS} WARN  plugin disabled plugin=ner kind=detector\n{TS} INFO  request served request_id={UUID} status=200 latency_ms={MS}\n{TS} ERROR upstream timeout provider=ds attempt=2",
    "commit {SHA}\nDate:   Tue Nov 4 10:12:33 2026 +0800\n\n    fix(redact): keep never_mask matches case-insensitive\n\n    The dictionary pass lower-cased terms but never_mask compared raw\n    values, so mixed-case entries slipped through.",
    "Refactor `handleChatCompletion` so the streaming branch and the buffered branch share the same redaction call. Keep the public signature unchanged and don't touch the metering code.",
    "```bash\n$ kubectl get pods -n gateway\nNAME                         READY   STATUS             RESTARTS   AGE\ngatehouse-7d9f8c6b5-x2kqp    1/1     Running            0          3d\ngatehouse-7d9f8c6b5-mz8tw    0/1     CrashLoopBackOff   {N}         {N}m\n```\nWhy is the second pod crash looping?",
    "Explain the difference between Kafka consumer groups and Redis Streams consumer groups, and when I should pick Cassandra over Postgres for an append-only audit log.",
    "We use Jenkins for CI, Grafana for dashboards and Jaeger for tracing. Write a short onboarding note listing what each tool is for.",
    "```js\nimport express from 'express';\nimport helmet from 'helmet';\nconst app = express();\napp.use(helmet());\napp.get('/healthz', (_req, res) => res.json({ status: 'ok' }));\napp.listen(process.env.PORT ?? {PORT});\n```\nAdd graceful shutdown on SIGTERM.",
    "The checksum of the release tarball is {HEX64}. How do I verify it with shasum on macOS?",
    "Trace id {UUID} shows the span `redact.detect` took {MS} ms. Where should I add more spans?",
    "```yaml\nredaction:\n  mode_by_target:\n    local: audit\n    external: mask\n  categories:\n    phone: mask\n    secret: block\n  fail_closed: true\n```\nIs `fail_closed` the default already?",
    "```python\nimport pytest\n\n@pytest.mark.parametrize('raw,expected', [('  a ', 'a'), ('', '')])\ndef test_strip(raw, expected):\n    assert raw.strip() == expected\n```\npytest reports `1 passed, 1 xfailed`. Why xfailed?",
    "```go\nfunc (v *Vault) Put(key, placeholder, value string) {\n\tv.mu.Lock()\n\tdefer v.mu.Unlock()\n\tv.m[key] = entry{placeholder: placeholder, value: value, at: time.Now()}\n}\n```\nIs there a race if Get is called concurrently?",
    "Rename the Django app `accounts` to `members` without losing migrations. We're on Django 5.1 and Postgres 16.",
    "Write a Rust function that parses `KEY=VALUE` lines from a .env file and ignores comments. Return a HashMap<String, String>.",
    "git rebase -i HEAD~{N} says `error: could not apply {SHORT}... feat(admin): add branding menu`. How do I continue after resolving conflicts?",
    "```dockerfile\nFROM golang:1.26 AS build\nWORKDIR /src\nCOPY . .\nRUN CGO_ENABLED=0 go build -o /out/gatehouse ./cmd/gatehouse\nFROM gcr.io/distroless/static\nCOPY --from=build /out/gatehouse /gatehouse\nENTRYPOINT [\"/gatehouse\", \"serve\"]\n```\nCan I make the image smaller?",
    "panic: runtime error: index out of range [{N}] with length {N}\n\ngoroutine {N} [running]:\ngithub.com/yiyiSmallti/precinct-gatehouse/internal/redact.(*Engine).detectSegment(...)\n\t/src/internal/redact/redact.go:{N} +0x2c4\nmain.main()\n\t/src/cmd/gatehouse/main.go:{N} +0x1a8",
    "What does `ORT_API_VERSION` mean in onnxruntime_c_api.h, and why does GetApi return NULL on an older dylib?",
    "In Swift, how do I make an actor-isolated property readable from a nonisolated context without warnings in Swift 6 strict concurrency?",
    "```sql\nSELECT route_backend, route_reason, count(*)\nFROM request_log\nWHERE created_at > datetime('now', '-1 day')\nGROUP BY 1, 2\nORDER BY 3 DESC;\n```\nAdd the p95 of latency_ms per backend.",
    "Compare Hugo, Gatsby and Astro for a documentation site that needs i18n and full-text search.",
    "The function `getUserByName` returns nil when the name has trailing whitespace. Should normalization happen in the handler or in the repository layer?",
    "Our Terraform plan wants to destroy `aws_s3_bucket.audit_logs`. The state shows id {UUID}. How do I import it instead?",
    "Benchmark output:\n```\nBenchmarkDetect/short-10      {N}   {MS} ns/op   {N} B/op   {N} allocs/op\nBenchmarkDetect/long-10       {N}   {MS} ns/op   {N} B/op   {N} allocs/op\n```\nWhere do the allocations come from?",
    "Rewrite this commit message in conventional-commit style: 'changed stuff in the router so tiered works in shadow mode and fixed the test'.",
    "```ts\ntype Message = { role: 'system' | 'user' | 'assistant' | 'tool'; content: string };\nexport function lastUserTurn(msgs: Message[]): Message | undefined {\n  return [...msgs].reverse().find((m) => m.role === 'user');\n}\n```\nIs the spread necessary?",
    "Placeholder config for the README:\n```env\nOPENAI_API_KEY=<your-key-here>\nDATABASE_URL=postgres://USER:PASSWORD@HOST:5432/DB\n```\nIs this clear enough for new contributors?",
    "The UUID {UUID} appears twice in the audit log. Can request_id collide, or is this a retry?",
    "Explain how SentencePiece Unigram tokenization picks the best segmentation with Viterbi, and why scores are log probabilities.",
    "Lint output:\n```\ninternal/live/manager.go:{N}:{N}: S1008: should use 'return x == y' instead of 'if x == y { return true }; return false' (gosimple)\ninternal/admin/api.go:{N}:{N}: ineffectual assignment to err (ineffassign)\n```",
    "Make the Ruby on Rails job `NightlyDigestJob` idempotent using a Redis lock with a 10 minute TTL.",
    "How do I tell Claude Code to stop running the full test suite after every edit? I only want `go test ./internal/redact/...`.",
    "```c\nstatic int parse_header(const char *buf, size_t len, struct header *h) {\n    if (len < 8) return -EINVAL;\n    memcpy(&h->magic, buf, 4);\n    memcpy(&h->size, buf + 4, 4);\n    return 0;\n}\n```\nIs this endian-safe?",
    "The Kubernetes ingress returns 502 only for /v1/messages with stream=true. nginx logs show `upstream prematurely closed connection`. What should I check?",
    "Write a migration that adds a nullable `route_reason TEXT` column to request_log and backfills it with 'legacy'.",
    "Why does `python -m venv .venv && source .venv/bin/activate` not change `which python` inside a tmux pane?",
    "Summarize the SPEC section on plugin interfaces in three bullet points for a design review.",
    "Our Grafana panel uses `histogram_quantile(0.95, sum(rate(gatehouse_latency_bucket[5m])) by (le))`. Why is it flat at 10s?",
    "```kotlin\ndata class Order(val id: Long, val items: List<Item>, val status: Status = Status.NEW)\nfun Order.total(): BigDecimal = items.sumOf { it.price * it.qty.toBigDecimal() }\n```\nConvert to Java 21 records.",
    "build failed:\n```\n# github.com/yalue/onnxruntime_go\ncgo: C compiler \"gcc\" not found: exec: \"gcc\": executable file not found in $PATH\n```",
    "```go\ntype AccountService struct {\n\trepo   AccountRepo\n\tlogger *slog.Logger\n}\n\nfunc (s *AccountService) Disable(ctx context.Context, accountID int64) error {\n\treturn s.repo.SetStatus(ctx, accountID, StatusDisabled)\n}\n```\nAdd an audit event when an account is disabled.",
    "```python\nPASSWORD_MIN_LENGTH = 12\n\ndef validate_password(password: str) -> list[str]:\n    errors = []\n    if len(password) < PASSWORD_MIN_LENGTH:\n        errors.append('too short')\n    return errors\n```\nAdd checks for digits and symbols.",
    "```ts\nconst SECRET_KEY_ENV = 'APP_SECRET_KEY';\nexport function loadSecret(): string {\n  const v = process.env[SECRET_KEY_ENV];\n  if (!v) throw new Error(`${SECRET_KEY_ENV} is not set`);\n  return v;\n}\n```\nShould this fall back to a file?",
    "Error: connect ECONNREFUSED 127.0.0.1:{PORT}\n    at TCPConnectWrap.afterConnect [as oncomplete] (node:net:1607:16) {\n  errno: -61,\n  code: 'ECONNREFUSED',\n  syscall: 'connect'\n}",
    "Traceback (most recent call last):\n  File \"/app/worker.py\", line {N}, in run\n    result = handler(payload)\n  File \"/app/handlers.py\", line {N}, in handler\n    return payload['items'][0]\nIndexError: list index out of range",
    "```\n$ go vet ./...\n# github.com/yiyiSmallti/precinct-gatehouse/internal/tiered\ninternal/tiered/hub.go:{N}:{N}: call has possible Printf formatting directive %d\n```\nIs this a real bug?",
    "Write a GitHub Actions workflow that runs `go test ./...` on ubuntu-latest and macos-latest, caches the module download, and uploads the e2e report as an artifact.",
    "```rust\npub fn checksum(data: &[u8]) -> u32 {\n    data.iter().fold(0u32, |acc, b| acc.wrapping_mul(31).wrapping_add(*b as u32))\n}\n```\nIs this good enough for deduplicating cache keys?",
    "How should I structure a monorepo with a Go backend, a Vue admin UI and a shared protobuf package? We use Buf for codegen.",
    "Explain what `GOMAXPROCS` does in a container with a 2-CPU quota and whether I still need automaxprocs on Go 1.26.",
    "The `Person` table has columns first_name, last_name, created_at. Write a SQLAlchemy model and an Alembic migration for it.",
    "```java\n@RestController\n@RequestMapping(\"/api/users\")\npublic class UserController {\n    @GetMapping(\"/{id}\")\n    public UserDto get(@PathVariable Long id) { return service.find(id); }\n}\n```\nAdd validation and a 404 when the user is missing.",
    "Our ESLint config extends `airbnb` and `prettier`. After upgrading to ESLint 9 flat config nothing is linted. What changed?",
    "```\nWARNING: The requested image's platform (linux/amd64) does not match the detected host platform (linux/arm64/v8)\nexec /usr/local/bin/gatehouse: exec format error\n```\nHow do I build a multi-arch image?",
    "Is it safe to log the `Authorization` header length (not value) for debugging? We want to spot empty tokens.",
    "Write a Bash function `retry` that retries a command up to N times with exponential backoff starting at 1s.",
    "What's the difference between `context.WithTimeout` and `context.WithDeadline`, and which should the plugin host use for Detect calls?",
    "```swift\nstruct Settings: Codable {\n    var theme: String = \"system\"\n    var fontSize: Int = 13\n}\n```\nPersist this with AppStorage instead of a JSON file.",
    "Profile shows 40% of CPU in `runtime.mallocgc` during detection. Should I pool the rune index slices?",
    "Explain BIO tagging for named entity recognition and how to merge sub-word predictions back to character spans.",
    "In ONNX Runtime, what is the difference between intra_op_num_threads and inter_op_num_threads for a single BERT model?",
    "```makefile\nbuild:\n\tgo build -o bin/gatehouse ./cmd/gatehouse\ntest:\n\tgo test ./...\n```\nAdd a `cross` target for linux/amd64 and windows/amd64.",
    "Our Sentry shows `TypeError: Cannot read properties of undefined (reading 'map')` in `ModelList.vue` line {N}. The API returned 204.",
    "Upgrade path from Vue 2.7 to Vue 3.5 for an admin panel with Vuex and Element UI — what breaks first?",
    "```\nERROR: duplicate key value violates unique constraint \"tokens_name_key\"\nDETAIL:  Key (name)=(ci-runner) already exists.\n```\nShould token names be unique per user instead?",
    "We see `x509: certificate signed by unknown authority` when the gateway calls the upstream behind a corporate proxy. Where do I add the CA bundle?",
    "Design a JSON schema for detector findings: kind, start, end, confidence, source. Offsets are Unicode code points.",
    "```go\nfor i := range text {\n\tidx = append(idx, i)\n}\n```\nThis builds a rune-to-byte index. Is there a faster way for ASCII-only text?",
    "Write release notes for {VER}: zero-downtime upgrades via FD handoff, persistent admin sessions, and a fix for unknown YAML fields.",
    "Why does `sort.SliceStable` matter when findings with the same start are ordered longest first?",
    "Cron expression for every weekday at 02:30 UTC, and how to express the same in a systemd timer.",
    "```\n$ du -sh ~/Library/Caches/* | sort -h | tail -3\n1.2G\t/Users/dev/Library/Caches/go-build\n2.9G\t/Users/dev/Library/Caches/Homebrew\n5.4G\t/Users/dev/Library/Caches/JetBrains\n```\nWhich of these is safe to delete?",
    "Explain Raft leader election timeouts and why they are randomized.",
    "Implement an LRU cache in Go with generics and a TTL per entry, safe for concurrent use.",
    "Our protobuf field `int32 start = 3;` overflows for very long texts. Is changing it to int64 wire-compatible?",
    "```html\n<form method=\"post\" action=\"/login\">\n  <input name=\"username\" autocomplete=\"username\">\n  <input name=\"password\" type=\"password\" autocomplete=\"current-password\">\n</form>\n```\nAdd CSRF protection.",
    "Measure p50/p95 latency of a function in Go without pulling in a metrics library.",
    "What happens to in-flight requests when SIGHUP triggers a config reload and the snapshot swaps?",
]

NEG_ZH = [
    "```go\n// Detect 扫描文本并返回命中的片段，偏移是字节偏移。\nfunc (e *Engine) Detect(ctx context.Context, text string) ([]Finding, error) {\n\tif text == \"\" {\n\t\treturn nil, nil\n\t}\n\treturn e.detectSegment(ctx, text)\n}\n```\n这里空字符串直接返回 nil 会不会有问题？",
    "帮我把这段 Python 改成异步的：\n```python\nimport requests\n\ndef fetch_all(urls):\n    return [requests.get(u, timeout=5).json() for u in urls]\n```",
    "跑 `go test ./...` 的输出：\n```\n--- FAIL: TestTieredShadow ({DUR})\n    g12_tiered_test.go:{N}: 期望 route_backend=gpu1-1m，实际为空\nFAIL\n```\n帮我看看是不是 shadow 模式没写日志。",
    "日志：\n{TS} INFO  启动完成 监听 127.0.0.1:{PORT}\n{TS} WARN  检测器插件未启用 name=ner\n{TS} ERROR 上游超时 provider=ds 重试次数=2\n这个告警需要处理吗？",
    "解释一下张量并行和流水线并行的区别，以及在 8 卡机器上推理 DeepSeek 时怎么选。",
    "李代数和李群在机器人姿态估计里分别起什么作用？给个直观解释。",
    "高斯过程回归和贝叶斯优化是什么关系？帮我写个最小的 Python 例子。",
    "提交信息帮我改成规范格式：修了路由在 shadow 模式下的 bug，顺便改了测试。",
    "```ts\n// 把消息数组里最后一条用户消息取出来\nexport function lastUser(msgs: Msg[]) {\n  return msgs.filter((m) => m.role === 'user').at(-1);\n}\n```\n这样写性能有问题吗？",
    "我们用飞书做通知、钉钉做审批、企业微信对接客户，帮我设计一个统一的消息推送接口。",
    "鸿蒙 NEXT 上的 ArkTS 和 TypeScript 有哪些不兼容的地方？",
    "通义千问、文心一言和 DeepSeek 的 API 都兼容 OpenAI 格式吗？写个统一的客户端封装。",
    "```yaml\n# 脱敏配置\nredaction:\n  mode_by_target:\n    local: audit   # 本地模型只审计\n    external: mask # 外部模型必须脱敏\n```\n注释写得清楚吗？",
    "这个 SHA256 校验和是 {HEX64}，在 Linux 上怎么校验下载的文件？",
    "请求 ID {UUID} 在审计日志里出现了两次，是重试导致的吗？",
    "```bash\n$ git log --oneline -3\n{SHORT} fix(redact): 修复词典大小写问题\n{SHORT} feat(ui): 增加万/百万/亿单位选择\n{SHORT} docs: 清理内部地址\n```\n帮我把这三条合成一条。",
    "解释一下 Kafka 的分区再均衡机制，消费者组里有人掉线会发生什么？",
    "帮我写一个 Go 的令牌桶限流器，支持每秒 {N} 个请求，突发 {N} 个。",
    "Redis 集群模式下 MGET 跨槽报错 CROSSSLOT，怎么改？",
    "```sql\n-- 按天统计请求量\nSELECT date(created_at) AS d, count(*) FROM request_log GROUP BY d ORDER BY d DESC LIMIT 7;\n```\n再加一列平均延迟。",
    "构建报错：\n```\n# github.com/yalue/onnxruntime_go\ncgo: C compiler \"gcc\" not found\n```\nmacOS 上怎么装编译器？",
    "这段正则 `(?<![0-9\\s])\\s+` 在 Go 里不支持，有什么替代写法？",
    "我想把单体服务拆成路由、脱敏、计量三个模块，接口怎么定义比较好？先给个目录结构。",
    "React 18 的并发渲染下 useEffect 会执行两次，这是 StrictMode 导致的吗？",
    "解释一下为什么 SQLite 在 WAL 模式下读写可以并发，但写写不行。",
    "Docker 镜像太大了，有 1.2GB，帮我看看多阶段构建怎么写。",
    "panic 信息：\n```\npanic: assignment to entry in nil map\n\ngoroutine {N} [running]:\nmain.(*Router).Activate(...)\n\t/src/internal/tiered/router.go:{N}\n```\n是哪里没初始化？",
    "pytest 输出 `{N} passed, 2 skipped in {DUR}`，被跳过的两个测试怎么看原因？",
    "帮我写一个 Makefile，包含 build、test、lint、交叉编译到 linux/amd64 和 windows/amd64 四个目标。",
    "```python\nclass 订单服务:\n    def 创建订单(self, 用户编号: int, 商品列表: list) -> dict:\n        # 校验库存后写库\n        return {\"状态\": \"已创建\", \"数量\": len(商品列表)}\n```\n中文标识符在生产代码里合适吗？",
    "解释一下 Transformer 里多头注意力的 QKV 是怎么算的，用 NumPy 写个最小实现。",
    "Nginx 反向代理 SSE 流式响应总是被缓冲，`proxy_buffering off` 加在哪一层？",
    "我们的网关平均请求有 13 万 token，增量检测缓存按消息哈希做，哈希用 SHA-256 还是 xxhash？",
    "帮我把这个 shell 脚本改成遇错即停：\n```bash\ncd build\nmake\ncp out/* /opt/app/\n```",
    "TypeScript 里 `satisfies` 和类型断言 `as` 有什么区别？各举一个例子。",
    "王者荣耀的匹配机制是怎么设计的？从分布式系统角度讲讲大概的架构。",
    "江湖上说 Rust 的所有权很难学，给我一个两周的学习计划。",
    "宋体和黑体在网页里怎么设置回退字体？CSS 怎么写。",
    "这个函数叫 `zhangLiangSplit`，其实是张量切分的意思，命名要不要改成英文全称？",
    "梯度裁剪的阈值一般设多少？训练 BERT 时 loss 出现 NaN 怎么排查。",
    "运行 `npm run build` 报 `JavaScript heap out of memory`，怎么调大 Node 的内存？",
    "版本号从 {VER} 升到 {VER}，按语义化版本应该算 minor 还是 patch？",
    "帮我解释这段 Go 泛型约束：`type Number interface { ~int | ~int64 | ~float64 }`。",
    "部署窗口定在周四晚上十点，帮我写一份变更通知模板，包含回滚步骤。",
    "监控显示 p99 延迟从 {MS} ms 涨到了 {N} ms，先从哪几个指标查起？",
    "```go\n// AccountRepo 负责账号表的读写。\ntype AccountRepo interface {\n\tFind(ctx context.Context, id int64) (*Account, error)\n\tSetStatus(ctx context.Context, id int64, s Status) error\n}\n```\n帮我写一个内存实现用于测试。",
    "```python\n# 密码强度校验：长度至少 12 位\ndef 校验密码(密码: str) -> bool:\n    return len(密码) >= 12\n```\n再加上必须包含数字和符号的规则。",
    "```ts\n// 从环境变量读取密钥，没有就报错\nconst key = process.env.APP_SECRET_KEY ?? '';\nif (!key) throw new Error('缺少 APP_SECRET_KEY');\n```\n这样写在 Edge Runtime 里能用吗？",
    "报错：\n```\nError: connect ECONNREFUSED 127.0.0.1:{PORT}\n```\n本地 Redis 明明起了，为什么连不上？",
    "Python 报错：\n```\nKeyError: 'items'\n  File \"/app/handlers.py\", line {N}, in handler\n```\n怎么写得更健壮一点？",
    "帮我写一个 GitHub Actions 工作流：在 ubuntu 和 macOS 上跑 `go test ./...`，缓存模块下载，并把 e2e 报告作为产物上传。",
    "Go 1.26 里容器只给 2 个 CPU 的时候，GOMAXPROCS 默认是多少？还需要 automaxprocs 吗？",
    "人员表有 first_name、last_name、created_at 三列，帮我写 GORM 模型和迁移。",
    "```java\n// 用户控制器：按编号查询用户\n@GetMapping(\"/users/{id}\")\npublic UserDto get(@PathVariable Long id) { return service.find(id); }\n```\n用户不存在时返回 404。",
    "ESLint 升级到 9 之后用 flat config，原来的 airbnb 规则全部不生效了，怎么迁移？",
    "镜像在 arm64 机器上报 `exec format error`，怎么用 buildx 打多架构镜像？",
    "调试时记录 Authorization 头的长度（不记录值）安全吗？我们想排查空令牌。",
    "写一个 Bash 的 retry 函数，最多重试 N 次，从 1 秒开始指数退避。",
    "插件宿主调用 Detect 应该用 context.WithTimeout 还是 WithDeadline？有什么区别？",
    "性能分析显示检测阶段 40% 的 CPU 花在 runtime.mallocgc，要不要用 sync.Pool 复用切片？",
    "解释一下命名实体识别里的 BIO 标注，以及子词预测怎么合并回字符区间。",
    "ONNX Runtime 的 intra_op_num_threads 和 inter_op_num_threads 有什么区别？单个 BERT 模型该怎么设？",
    "前端 Sentry 报 `Cannot read properties of undefined (reading 'map')`，出在 ModelList.vue 第 {N} 行，接口返回的是 204。",
    "管理后台从 Vue 2.7 升到 Vue 3.5，用了 Vuex 和 Element UI，最先会坏的是哪些地方？",
    "```\nERROR: duplicate key value violates unique constraint \"tokens_name_key\"\n```\n令牌名字是不是应该按用户唯一？",
    "网关在公司代理后面访问上游报 `x509: certificate signed by unknown authority`，CA 证书加在哪里？",
    "帮我设计检测结果的 JSON Schema：类别、起止偏移、置信度、来源，偏移按 Unicode 码点算。",
    "帮我写 {VER} 的发布说明：FD 交接零停机升级、管理会话持久化、修复未知 YAML 字段报错。",
    "为什么起点相同的检测结果要按长度降序稳定排序？sort.SliceStable 在这里起什么作用？",
    "每个工作日 UTC 02:30 执行的 cron 表达式怎么写？换成 systemd timer 怎么写？",
    "解释一下 Raft 的选主超时为什么要随机化。",
    "用 Go 泛型实现一个带过期时间的并发安全 LRU 缓存。",
    "protobuf 里 `int32 start = 3;` 改成 int64 是线上兼容的吗？",
    "这个登录表单要加 CSRF 防护：\n```html\n<input name=\"username\">\n<input name=\"password\" type=\"password\">\n```",
    "Go 里不引入监控库，怎么统计一个函数的 p50 和 p95 延迟？",
    "SIGHUP 触发配置热重载时，正在处理的请求会受影响吗？快照切换是怎么做的？",
    "```\n$ du -sh ~/Library/Caches/* | sort -h | tail -2\n2.9G\t/Users/dev/Library/Caches/Homebrew\n5.4G\t/Users/dev/Library/Caches/JetBrains\n```\n这些缓存哪些可以安全删除？",
    "单测覆盖率从 {N}% 掉到了 {N}%，帮我找出这次提交里没有测试的函数。",
    "帮我把 Prometheus 的告警规则改成：5 分钟内 5xx 比例超过 2% 就告警。",
    "Vite 打包后的 chunk 超过 500KB 警告怎么处理？按路由拆分怎么配？",
    "解释一下 HTTP/2 的流控窗口，为什么大响应在代理后面会卡住。",
    "gRPC 默认最大消息 4MB，13 万 token 的文本会超吗？怎么调大？",
    "帮我写一个 SQLite 的 VACUUM 定时任务，避免 WAL 文件无限增长。",
]

# ---------------------------------------------------------------- requests

SYSTEM_PROMPT = (
    "You are a coding assistant running in the user's terminal. You can read files, run shell commands and edit code. "
    "Follow the repository's conventions, keep changes minimal, and explain what you changed. Never commit unless asked. "
    "When you run tests, report failures verbatim. Prefer rg over grep. The working directory is a Go module named "
    "github.com/yiyiSmallti/precinct-gatehouse. Today's date is 2026-11-25. Respond in the user's language.\n\n"
    "# Tools\n- Read(file_path): read a file\n- Bash(command): run a shell command\n- Edit(file_path, old, new): replace text\n\n"
    "# Style\nUse short paragraphs. Reference code as path:line. Do not add comments that restate the code."
)

TOOL_FILE = (
    "package config\n\n// Redaction configures the redaction stage.\ntype Redaction struct {\n"
    "\tModeByTarget map[string]string `yaml:\"mode_by_target,omitempty\"`\n\tCategories map[string]string `yaml:\"categories,omitempty\"`\n"
    "\tDictionary string `yaml:\"dictionary,omitempty\"`\n\tNeverMask []string `yaml:\"never_mask,omitempty\"`\n"
    "\tDetectors []string `yaml:\"detectors,omitempty\"`\n\tFailClosed *bool `yaml:\"fail_closed,omitempty\"`\n}\n\n"
    "// FailClosedOn reports whether a detector failure blocks the request.\nfunc (r Redaction) FailClosedOn() bool {\n"
    "\treturn r.FailClosed == nil || *r.FailClosed\n}\n"
)


def request_samples():
    out = []
    user_clean = [
        ("en", "Add a `min_confidence` field to the detector config and wire it through. Don't change the plugin protocol."),
        ("zh", "帮我给检测器配置加一个 min_confidence 字段，别改插件协议。"),
        ("en", "Why does the e2e test G2 fail only on Windows?"),
        ("zh", "这个测试只在 Windows 上失败，帮我查一下原因。"),
    ]
    user_pii = [
        ("zh", "顺便把通知邮件的署名改成{ZPER}，地址写{ZADDR}。"),
        ("en", "Also set the support contact in the footer to {EPER}."),
        ("zh", "我用 {SECRET} 调接口一直 401，先帮我看看这个。"),
        ("en", "My username on the staging box is {USER}, run the migration as that user."),
        ("mixed", "PM {EPER} 说 release date 改了，我的 birthday {ZDATE} 那天别安排上线。"),
    ]
    for i in range(24):
        lang, u1 = user_clean[i % len(user_clean)]
        segs = [{"role": "system", "text": SYSTEM_PROMPT, "entities": []},
                {"role": "user", "text": u1, "entities": []},
                {"role": "assistant", "text": "I'll read the config package first." if lang == "en" else "我先看一下 config 包。", "entities": []},
                {"role": "tool", "text": "internal/config/config.go\n" + TOOL_FILE, "entities": []},
                {"role": "tool", "text": neg_fill(R.choice(NEG_EN[3:7] + NEG_EN[22:24])), "entities": []},
                {"role": "assistant", "text": neg_fill(R.choice(NEG_ZH if lang == "zh" else NEG_EN)), "entities": []}]
        clean = i % 2 == 0
        if not clean:
            plang, tmpl = user_pii[i % len(user_pii)]
            t, ents = fill(tmpl)
            segs.append({"role": "user", "text": t, "entities": ents})
            lang = plang if plang == "mixed" else lang
        else:
            segs.append({"role": "user", "text": neg_fill(R.choice(NEG_ZH if lang == "zh" else NEG_EN)), "entities": []})
        out.append({"id": f"req-{i:03d}", "group": "request", "lang": lang, "category": "clean" if clean else "mixed",
                    "in_fence": False, "segments": segs})
    return out


# ---------------------------------------------------------------- main

PER_CELL = 34


def main():
    out_dir = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "..", "data")
    os.makedirs(out_dir, exist_ok=True)
    pos = []
    for (cat, lang), tmpls in POS.items():
        for k in range(PER_CELL):
            tmpl = tmpls[k % len(tmpls)]
            text, ents = fill(tmpl)
            for e in ents:
                assert text[e["start"]:e["end"]] == e["text"]
            pos.append({"id": f"pos-{cat}-{lang}-{k:02d}", "group": "positive", "lang": lang, "category": cat,
                        "in_fence": "```" in tmpl, "text": text, "entities": ents})
    neg = []
    for lang, pool in (("en", NEG_EN), ("zh", NEG_ZH)):
        assert len(set(pool)) == len(pool) >= 80, (lang, len(pool))
        for k, tmpl in enumerate(pool):
            t = neg_fill(tmpl)
            neg.append({"id": f"neg-{lang}-{k:03d}", "group": "negative", "lang": lang, "category": "clean",
                        "in_fence": "```" in t, "text": t, "entities": []})
    req = request_samples()
    for name, rows in (("positives.jsonl", pos), ("negatives.jsonl", neg), ("requests.jsonl", req)):
        with open(os.path.join(out_dir, name), "w", encoding="utf-8") as f:
            for r in rows:
                f.write(json.dumps(r, ensure_ascii=False) + "\n")
        print(f"{name}: {len(rows)}")


if __name__ == "__main__":
    main()
