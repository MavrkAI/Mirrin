"""Wake phrases, spellings, near-misses and evaluation sentences for each model.

All text here is written for this project (no third-party word lists). Spelling
variants only coax a TTS voice into the intended pronunciation. Both engines take
espeak-style IPA inline as [[...]] (Piper natively, Kokoro via common.TTS).

espeak reads the spelling "Mirrin" as /mˈɜːɹɪn/ ("Murrin") and "Mirren"/"Miran" as
/mˈɜːɹən/, so the intended /mˈɪɹɪn/ comes from "Mirin" and from phonemes; the literal
spellings stay in as rarer variants, since some people will read the name that way.
"say" is how each name is spoken inside longer evaluation and continuation sentences.
"""

CONTINUATIONS = [
    "set a timer for five minutes", "what's the weather like", "play some jazz",
    "turn off the kitchen lights", "add bread to the list", "how long until dinner",
    "read my messages", "call Sam", "what's next on my calendar", "skip this song",
    "is it going to rain", "send that to Priya",
]
# Evaluation continuations are disjoint from the training ones.
EVAL_CONTINUATIONS = ["what's on this afternoon", "remind me to call mum", "how's my inbox looking"]

# Speech that should never wake any model (spoken by TTS voices so that "sounds
# synthetic" is never a cue for the positive class). LibriSpeech transcripts add more.
GENERIC = [
    "what time is it", "turn the lights off", "hey everyone, listen up", "hey there",
    "the weather is lovely today", "call the accountant at three", "email priya the deck",
    "hey mum", "a very quick brown fox", "hey siri", "ok google", "alexa, stop", "hey jarvis",
    "hey cortana", "hey google", "hey computer", "hey", "never mind", "play some music",
    "how far is the airport", "can you remind me tomorrow", "i'm heading out for a bit",
    "hey, have a look at this", "hey you", "hey buddy", "hey honey", "hey guys",
]

MODELS = {
    "hey_mirrin": {
        "name": "Mirrin",
        "say": "[[mˈɪɹɪn]]",
        "positive_text": ["Hey Mirin", "Hey, Mirin.", "hey mirin", "Hey Mirin!", "hey, Mirin?",
                          "Hey [[mˈɪɹɪn]]", "Hey, [[mˈɪɹɪn]].", "Hey [[mˈɪɹən]]"],
        # the literal spellings (read by espeak as "Murrin"/"Murren"), used sparingly
        "positive_rare": ["Hey Mirrin", "Hey, Mirrin.", "Hey Mirren", "Hey Miran"],
        "positive_phonemes": ["hˈeɪ mˈɪɹɪn", "hˈeɪ, mˈɪɹɪn.", "heɪ mˈɪɹən!"],
        "near_miss": [
            "hey mirror", "hey, mirror on the wall", "hey Miriam", "hey Myron", "hey Karen",
            "hey Darren", "hey Merlin", "hey Marion", "hey Erin", "hey Myrna", "hey Warren",
            "hey Kieran", "hey Morin", "hey Milan", "hey Mary", "hey Robin",
            "hey Martin", "hey Mervyn", "hey merry men", "hey, mirroring",
            "add mirin to the shopping list", "a splash of mirin and soy sauce",
            "we're out of mirin", "mirin is a sweet rice wine",
            "I told [[mˈɪɹɪn]] about it yesterday", "ask [[mˈɪɹɪn]] later", "[[mˈɪɹɪn]] is busy right now",
            "the new assistant is called [[mˈɪɹɪn]]", "is [[mˈɪɹɪn]] listening", "I told Mirrin about it",
            "look in the mirror", "the mirror is cracked", "Karen and Darren are here",
        ],
        "hey_near": ["mirror", "miriam", "myron", "karen", "darren", "merlin", "marion",
                     "erin", "myrna", "warren", "kieran", "robin"],
        "eval_near_miss": [
            "hey mirror, mirror", "hey Miriam, remind me to call mum", "hey Myron",
            "pass the mirin please", "I'll ask [[mˈɪɹɪn]] tomorrow", "hey Darren, lights",
            "hey Merlin", "hey Erin, what's up", "hey Marion", "hey Karen",
        ],
    },
    "hey_nyra": {
        "name": "Nyra",
        "say": "Nyra",
        "positive_text": ["Hey Nyra", "Hey, Nyra.", "hey nyra", "Hey Nyra!", "Hey Nyrah", "hey, Nyra?"],
        # "Naira" as espeak reads it is /nˈɛɹə/; the currency is said "NYE-ra" or "nye-RAH"
        "positive_rare": ["Hey [[nˈaɪɹɑː]]", "Hey [[naɪɹˈɑː]]"],
        "positive_phonemes": ["hˈeɪ nˈaɪɹə", "hˈeɪ, nˈaɪɹə.", "heɪ nˈaɪɹə!"],
        "near_miss": [
            "hey near", "hey, near a", "come near", "hey, Nairobi", "Nairobi", "hey Myra",
            "hey Tyra", "Nirav", "hey Nirav", "hey Kyra", "hey Ira", "hey Lyra", "hey Moira",
            "hey Nina", "hey Nora", "hey Mira", "hey Keira", "hey Sarah", "hey Nyla",
            "hey hire a", "hire a car", "hey Naomi", "hey Dana", "hey nadir", "hey, nice try",
            "hey Myra, what's up", "hey Tyra Banks", "hey Kyra, come here",
            "I told Nyra about it", "ask Nyra later", "Nyra is busy right now",
        ],
        "hey_near": ["myra", "tyra", "kyra", "ira", "lyra", "nina", "nora", "mira", "keira",
                     "nyla", "naomi", "nirav", "near"],
        "eval_near_miss": [
            "hey near the station", "Nairobi is lovely", "hey Myra, remind me to call mum",
            "hey Tyra", "Nirav called", "hey Kyra", "hey Ira, lights", "hey Nina",
            "I'll ask Nyra tomorrow", "hey Moira",
        ],
    },
    "hey_pickoo": {
        "name": "Pickoo",
        "say": "Pickoo",
        "positive_text": ["Hey Pickoo", "Hey, Pickoo.", "hey pickoo", "Hey Pickoo!", "Hey Picku",
                          "Hey Pikoo", "hey, Pickoo?"],
        "positive_rare": ["Hey Peekoo"],
        "positive_phonemes": ["hˈeɪ pˈɪkuː", "hˈeɪ, pˈɪkuː.", "hˈeɪ pˈiːkuː"],
        "near_miss": [
            "pick you", "hey, pick you up", "I'll pick you up", "pick up", "hey pick up",
            "pick it up", "pikachu", "hey pikachu", "peekaboo", "hey peekaboo", "hey picky",
            "piccolo", "hey piccolo", "hey picasso", "hey pickle", "hey kiku", "hey Pico",
            "hey pick it", "hey, pick a card", "picnic", "hey Nicko", "hey Vicky",
            "hey pick one", "hey Mickey", "hey cuckoo", "hey Pooh", "hey pick two",
            "hey Becky", "hey Kiki", "hey pick you", "hey Ricky",
            "I told Pickoo about it", "ask Pickoo later",
        ],
        "hey_near": ["picky", "pickle", "pico", "piccolo", "nicko", "vicky", "mickey", "becky",
                     "kiki", "ricky", "cuckoo", "pooh", "pick"],
        "eval_near_miss": [
            "I'll pick you up at six", "pick up the phone", "pikachu is cute", "peekaboo",
            "hey picky eater", "the piccolo is loud", "hey Vicky", "hey Mickey",
            "I'll ask Pickoo tomorrow", "hey pickle",
        ],
    },
}

# Evaluation positives: the plain phrase and the phrase running straight into a request.
def eval_positive_text(model):
    n = MODELS[model]["say"]
    lower = n if n.startswith("[[") else n.lower()
    return [f"Hey {n}", f"Hey, {n}.", f"hey {lower}"] + [f"Hey {n}, {c}." for c in EVAL_CONTINUATIONS]
