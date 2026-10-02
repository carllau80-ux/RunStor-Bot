随二进制一起分发的字体
======================

本项目在二进制里内置两份字体, 因为中文报告需要**同时**有中文字形和拉丁字形,
而没有任何一份系统自带字体同时满足 (见下).

DejaVuSans.ttf —— 拉丁字母、数字、符号
--------------------------------------
来源: Debian 软件包 fonts-dejavu-core, /usr/share/fonts/truetype/dejavu/DejaVuSans.ttf
sha256 = ae7b7855e115a5966d8b1b3f80f254ccc117ec86f9965e202ee2940453837280

    版权: Copyright (c) 2003 by Bitstream, Inc. All Rights Reserved.
          Bitstream Vera 是 Bitstream, Inc. 的商标。
          DejaVu 的改动已进入公有领域。
    许可: bitstream-vera (允许复制与分发字体软件, 包括修改版; 不得单独出售字体本身,
          且须保留版权声明)。完整文本见 Debian 版权文件
          /usr/share/doc/fonts-dejavu-core/copyright。

DroidSansFallbackFull.ttf —— 中日韩字形
----------------------------------------
来源: Debian 软件包 fonts-droid-fallback, /usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf
sha256 = acb6440a713d880a13a21b468ba7cd43f5a2b2934972e51be791c880730777b8

    版权: Copyright © 2006-2010 Google Corp. Droid 是 Google Corp. 的商标。
    许可: Apache License 2.0, 完整文本见 /usr/share/common-licenses/Apache-2.0
          或 http://www.apache.org/licenses/LICENSE-2.0

为什么随二进制一起分发
----------------------
巡检报告是中文的, 而 PDF 里显示中文必须**嵌入字体** (PDF 内置的 14 种标准字体不含
任何中日韩字形)。可选做法有三种:

  1. 依赖目标机已装的中文字体 —— 部署目标 (Debian 13) 上只有 Noto Sans CJK, 那是
     CFF/OpenType 轮廓 (.ttc), 不是 TrueType 轮廓, 无法作为 CIDFontType2 嵌入;
  2. 让浏览器打印成 PDF —— 那样服务端产不出文件, 也不能作为附件归档;
  3. 把字体编进二进制 (本做法)。

所以选 3: 任何一台 Linux 上生成的报告都是同一个样子, 不依赖目标机装了什么字体。

为什么是两份
------------
DroidSansFallbackFull 是**只补中日韩**的 fallback 字体 —— 它的 cmap 里连 A-Z、0-9、
斜杠、百分号都没有 (实测 A/0// 查到的字形编号都是 0)。而报告里满是路径、IP、百分比、
日志片段, 全靠拉丁字母和数字。反过来 DejaVuSans 一个汉字也没有。两份合起来才覆盖
报告的字符集: 按需从覆盖该字符的那份字体取字形, 行内可以混排。

运行期可以用 config.toml 的 inspect.pdf_font 指定别的 TrueType 字体替换上面第一份。

许可检查
--------
两份字体的 OS/2.fsType 都允许嵌入 (Droid 为 0x0008 可编辑嵌入, DejaVu 为 0x0000 可安装)。
加载时会检查这一位, 若字体声明的许可为"禁止嵌入"(0x0002) 则拒绝使用并说明原因。
